// Package httpclient is the single outbound HTTP client shared by every
// component that talks to the internet.
//
// Be a polite network citizen. This is ethics and self-interest
// at once — impolite fetchers get their addresses banned, and then the archive
// stops growing. Everything here exists to keep this service welcome on
// servers it does not own:
//
//   - a per-host token bucket, so no single origin ever sees a burst
//   - a global concurrency cap, so the machine cannot be the burst either
//   - robots.txt, fetched once per host and cached
//   - an honest, contactable User-Agent, with no browser spoofing
//   - retries that honor Retry-After, and that give up rather than insist
//
// The per-host limit is the one that matters most. A global limit protects
// this machine from doing too much work at once but limits nothing that any
// individual server experiences, because a server only sees the traffic sent
// to it.
package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/temoto/robotstxt"
	"golang.org/x/time/rate"
)

// MaxResponseBytes caps any single response body. A feed or article larger
// than this is a malfunction or an attack, not content worth archiving.
const MaxResponseBytes = 10 << 20 // 10MB

// Defaults matching the politeness rules.
const (
	DefaultRPS         = 1.0
	DefaultConcurrency = 10
	DefaultMaxAttempts = 3

	// robotsTTL bounds how stale a cached robots.txt may be. A day is long
	// enough that the file is fetched about once per host per run, and short
	// enough that a site newly asking to be left alone is honored promptly.
	robotsTTL = 24 * time.Hour

	// maxRetryAfter caps how long a Retry-After is obeyed *inline*.
	//
	// Sleeping inside a request holds a worker slot and a global concurrency
	// token the whole time, so a server asking for two minutes would park
	// scarce capacity on one host that has explicitly said it is busy. Past
	// this threshold the response is returned unretried and the job's own
	// exponential backoff reschedules it — the queue is the right place to
	// wait minutes, not the HTTP client.
	maxRetryAfter = 5 * time.Second
)

// ErrDisallowedByRobots means the host's robots.txt forbids this path.
//
// It is a distinct error because it is not a failure: the site has said no,
// the answer will not change on retry, and the article should be recorded as
// skipped rather than failed.
var ErrDisallowedByRobots = errors.New("disallowed by robots.txt")

// Options configure a Client. The zero value of each field takes its default.
type Options struct {
	UserAgent   string
	DefaultRPS  float64
	Concurrency int
	MaxAttempts int
}

// Client is a configured, rate-limited HTTP client. It is safe for concurrent
// use, and there should be exactly one per process.
type Client struct {
	hc          *http.Client
	userAgent   string
	maxAttempts int

	// sem is the global concurrency cap: a token is held for the duration of
	// each request, including its retries.
	sem chan struct{}

	defaultRPS float64

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	robots   map[string]*robotsEntry

	// sleep and jitter are fields so that tests can make backoff instant and
	// deterministic. Production uses the real clock and real randomness.
	sleep  func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration
}

type robotsEntry struct {
	// data is nil when the host has no usable robots.txt, which means allow.
	data    *robotstxt.RobotsData
	fetched time.Time
}

// UserAgent builds the outbound identification string.
//
// Contactable and honest, with no browser spoofing: an operator who wants this
// archiver to stop should be able to work out who to ask. contactURL may be
// empty, in which case it is omitted rather than faked.
func UserAgent(version, contactURL string) string {
	if contactURL == "" {
		return fmt.Sprintf("tomekeeper/%s", version)
	}
	return fmt.Sprintf("tomekeeper/%s (+%s)", version, contactURL)
}

// New returns a client with the given options.
func New(opts Options) *Client {
	if opts.DefaultRPS <= 0 {
		opts.DefaultRPS = DefaultRPS
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		// Kept low deliberately: this is the per-host connection budget, and
		// the point of the fetcher is to be gentle with any single origin.
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}

	return &Client{
		hc: &http.Client{
			Transport: transport,
			// Total budget for one attempt, including redirects and body.
			Timeout: 30 * time.Second,
		},
		userAgent:   opts.UserAgent,
		maxAttempts: opts.MaxAttempts,
		sem:         make(chan struct{}, opts.Concurrency),
		defaultRPS:  opts.DefaultRPS,
		limiters:    make(map[string]*rate.Limiter),
		robots:      make(map[string]*robotsEntry),
		sleep:       sleepCtx,
		jitter:      defaultJitter,
	}
}

// Request is one outbound fetch.
type Request struct {
	URL    string
	Header http.Header

	// SkipRobots exempts this request from robots.txt.
	//
	// Set for feed polls only. A feed is something the reader explicitly
	// subscribed to, published in a format whose entire purpose is automated
	// consumption; refusing to poll it because the site's robots.txt disallows
	// crawlers would break the subscription the reader asked for. Article and
	// asset fetches are never exempt.
	SkipRobots bool
}

// SetHostRate overrides the request rate for one host, from a domain rule.
//
// Passing a non-positive rate restores the default. Existing limiters are
// updated in place so a rule change takes effect without a restart.
func (c *Client) SetHostRate(host string, rps float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if rps <= 0 {
		rps = c.defaultRPS
	}
	if l, ok := c.limiters[host]; ok {
		l.SetLimit(rate.Limit(rps))
		return
	}
	c.limiters[host] = newLimiter(rps)
}

// Get issues a GET request subject to robots.txt, rate limiting, and retries.
func (c *Client) Get(ctx context.Context, rawURL string, header http.Header) (*http.Response, error) {
	return c.Do(ctx, Request{URL: rawURL, Header: header})
}

// Do issues a request, applying every politeness rule.
//
// The caller owns the response body and must close it.
func (c *Client) Do(ctx context.Context, req Request) (*http.Response, error) {
	parsed, err := url.Parse(req.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing URL %q: %w", req.URL, err)
	}
	host := parsed.Host
	if host == "" {
		return nil, fmt.Errorf("URL %q has no host", req.URL)
	}

	// The global cap is taken first and held across retries, so that a host
	// being slow to answer cannot let the total in-flight count drift up.
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()

	if !req.SkipRobots {
		allowed, err := c.robotsAllows(ctx, parsed)
		if err != nil {
			// A robots.txt that cannot be fetched is treated as permissive.
			// The alternative — refusing to fetch anything from a host whose
			// robots.txt is briefly 500ing — stops the archive for a reason
			// the site never actually gave.
			allowed = true
		}
		if !allowed {
			return nil, fmt.Errorf("%s: %w", req.URL, ErrDisallowedByRobots)
		}
	}

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if err := c.waitForHost(ctx, host); err != nil {
			return nil, err
		}

		resp, err := c.attempt(ctx, req)
		if err != nil {
			lastErr = err
			// A transport error is usually transient: a dropped connection, a
			// DNS blip, a TLS handshake that timed out.
			if attempt == c.maxAttempts {
				break
			}
			if waitErr := c.sleep(ctx, c.jitter(backoff(attempt))); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		delay, retry := retryDelay(resp, attempt)
		if !retry || attempt == c.maxAttempts {
			return resp, nil
		}

		// The body has to be drained and closed before reusing the
		// connection, and it is about to be replaced anyway.
		drain(resp)

		if err := c.sleep(ctx, c.jitter(delay)); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", c.maxAttempts, lastErr)
}

func (c *Client) attempt(ctx context.Context, req Request) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", req.URL, err)
	}

	for k, values := range req.Header {
		for _, v := range values {
			httpReq.Header.Add(k, v)
		}
	}
	// Set last so a caller cannot accidentally override the honest identity.
	httpReq.Header.Set("User-Agent", c.userAgent)

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", req.URL, err)
	}
	return resp, nil
}

func (c *Client) acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) release() { <-c.sem }

// waitForHost blocks until this host's token bucket allows another request.
func (c *Client) waitForHost(ctx context.Context, host string) error {
	c.mu.Lock()
	limiter, ok := c.limiters[host]
	if !ok {
		limiter = newLimiter(c.defaultRPS)
		c.limiters[host] = limiter
	}
	c.mu.Unlock()

	if err := limiter.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for the %s rate limit: %w", host, err)
	}
	return nil
}

// newLimiter builds a token bucket allowing a small burst.
//
// A burst of one would serialize even the first two requests to a host that
// has not been touched in an hour, which is needlessly slow; a burst of three
// is still nothing a server would notice.
func newLimiter(rps float64) *rate.Limiter {
	return rate.NewLimiter(rate.Limit(rps), 3)
}

// robotsAllows reports whether the host's robots.txt permits this path for
// this User-Agent.
//
// It calls TestAgent rather than FindGroup followed by Test. The difference
// matters: the allow-all and disallow-all states a robots.txt can be in are
// only honored by TestAgent, while FindGroup returns an empty group for them,
// whose Test permits everything. Going through FindGroup would silently ignore
// a site that disallows crawling wholesale.
func (c *Client) robotsAllows(ctx context.Context, target *url.URL) (bool, error) {
	data, err := c.robotsData(ctx, target)
	if err != nil {
		return true, err
	}
	if data == nil {
		return true, nil
	}

	path := target.EscapedPath()
	if path == "" {
		path = "/"
	}
	return data.TestAgent(path, c.userAgent), nil
}

func (c *Client) robotsData(ctx context.Context, target *url.URL) (*robotstxt.RobotsData, error) {
	key := target.Scheme + "://" + target.Host

	c.mu.Lock()
	entry, ok := c.robots[key]
	c.mu.Unlock()

	if ok && time.Since(entry.fetched) < robotsTTL {
		return entry.data, nil
	}

	data, err := c.fetchRobots(ctx, key)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.robots[key] = &robotsEntry{data: data, fetched: time.Now()}
	c.mu.Unlock()

	return data, nil
}

// fetchRobots retrieves and parses a host's robots.txt.
//
// It goes through the host rate limiter like any other request — it is a real
// request to that server — but not through Do, which would recurse.
//
// A nil return means "no usable robots.txt", which is treated as allow.
func (c *Client) fetchRobots(ctx context.Context, origin string) (*robotstxt.RobotsData, error) {
	if err := c.waitForHost(ctx, hostOf(origin)); err != nil {
		return nil, err
	}

	resp, err := c.attempt(ctx, Request{URL: origin + "/robots.txt"})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	// A server error means the site is broken, not that it has asked to be
	// left alone. Google's crawler specification treats 5xx as a full
	// disallow; this deviates deliberately.
	//
	// The reasoning is that a crawler discovering the web at large should back
	// off when it cannot read the rules, while this service fetches pages a
	// specific person deliberately subscribed to. Halting a personal archive
	// because a host's robots.txt is briefly 500ing enforces a restriction the
	// site never actually expressed. 4xx is a full allow under both readings.
	if resp.StatusCode >= 500 {
		return nil, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return nil, err
	}

	robots, err := robotstxt.FromStatusAndBytes(resp.StatusCode, body)
	if err != nil {
		return nil, err
	}
	return robots, nil
}

// retryDelay decides whether a response should be retried and how long to wait.
//
// The politeness rules: retry 429 and 503 honoring Retry-After; never retry a 4xx other than
// 408 and 429. A 404 will still be a 404 in ten seconds, and retrying it is
// just another request the origin did not need to serve.
func retryDelay(resp *http.Response, attempt int) (time.Duration, bool) {
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			if d > maxRetryAfter {
				// Asked to wait longer than a worker should be parked.
				return 0, false
			}
			return d, true
		}
		return backoff(attempt), true

	case http.StatusRequestTimeout:
		return backoff(attempt), true
	}

	// Other 5xx are worth one more try; other 4xx are the server telling us
	// something that will not change.
	if resp.StatusCode >= 500 {
		return backoff(attempt), true
	}
	return 0, false
}

// parseRetryAfter handles both forms the header may take: delta-seconds, or an
// HTTP-date.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := time.Until(when); d > 0 {
			return d, true
		}
		// A date in the past means "you may retry now".
		return 0, true
	}
	return 0, false
}

// backoff is exponential in the attempt number, starting at one second.
func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<(attempt-1)) * time.Second
}

// defaultJitter spreads retries by up to 25%, so that several jobs that failed
// against the same host at the same moment do not all come back together.
func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// Retry spreading is a scheduling concern, not a secret. An adversary who
	// could predict this jitter would learn when a retry lands, which is neither
	// useful nor hidden — the request itself is about to arrive at their server.
	return d + time.Duration(rand.Int64N(int64(d)/4+1)) //nolint:gosec // jitter needs spread, not unpredictability
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// drain reads and closes a response body that is being discarded, so the
// connection can be reused rather than torn down.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

func hostOf(origin string) string {
	if u, err := url.Parse(origin); err == nil {
		return u.Host
	}
	return origin
}

// ReadBody reads a response body up to MaxResponseBytes.
//
// It reads one byte past the cap so that hitting the limit is reported as an
// error rather than silently truncating, which would hand a half-downloaded
// document to a parser and produce a confusing failure much further along.
func ReadBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if len(body) > MaxResponseBytes {
		return nil, fmt.Errorf("response exceeds the %d byte limit", MaxResponseBytes)
	}
	return body, nil
}
