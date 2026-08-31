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
//   - an honest, contactable User-Agent, never claiming to be a person at a
//     browser. A domain rule may override it per host, for origins that filter
//     on the shape of the string rather than on conduct — see SetHostUserAgent
//     for what that concession is and where its line is.
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
	"syscall"
	"time"

	"github.com/temoto/robotstxt"
	"golang.org/x/time/rate"

	"github.com/runlevel-six/tomekeeper/internal/metrics"
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

	// AllowPrivate names the non-public destinations this client may reach. The
	// zero value allows none, which is the default and the safe direction — see
	// private.go for what is refused and why.
	//
	// A test serving fixtures on loopback is the honest instance of the case this
	// exists for, so those tests pass LoopbackAllowance() rather than the guard
	// carrying an exemption that would also hold in production.
	AllowPrivate PrivateAllowance
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

	// userAgents overrides the identity sent to one host, from a domain rule.
	// Empty for every host that has not been given one, which is nearly all of
	// them.
	userAgents map[string]string

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

	// Two dialers over one set of timeouts: the guarded one refuses an address that
	// is not public, and the permissive one is reached only by a host name the
	// operator named in TOME_FETCH_ALLOW_PRIVATE. A name allowance has to be
	// honored here rather than in the hook, because the hook sees addresses and a
	// LAN name's address is a DHCP lease.
	timeouts := net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	permissive := timeouts
	guarded := timeouts
	allow := opts.AllowPrivate
	guarded.Control = func(_, address string, _ syscall.RawConn) error {
		return guardAddress(allow, address)
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if host, _, err := net.SplitHostPort(address); err == nil && allow.allowsHost(host) {
				return permissive.DialContext(ctx, network, address)
			}
			return guarded.DialContext(ctx, network, address)
		},
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
			// The explanation half of the guard above: the dial hook would refuse
			// this hop anyway, and refusing it here is what makes the reason name the
			// redirect instead of an address nobody typed. Go's own limit of ten
			// hops still applies underneath.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				return checkRedirect(allow, req.URL)
			},
		},
		userAgent:   opts.UserAgent,
		maxAttempts: opts.MaxAttempts,
		sem:         make(chan struct{}, opts.Concurrency),
		defaultRPS:  opts.DefaultRPS,
		limiters:    make(map[string]*rate.Limiter),
		robots:      make(map[string]*robotsEntry),
		userAgents:  make(map[string]string),
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

// SetHostUserAgent overrides the identity sent to one host, from a domain rule.
//
// Passing an empty string restores the default. Set at startup alongside the
// per-host rate limits, for the same reason and with the same caveat: a rule
// added later takes effect on the next restart.
//
// This is the deliberate exception to the no-spoofing rule in the package
// comment, and it is narrow on purpose. Some origins now filter on the *shape*
// of the User-Agent rather than on behavior — measured against arstechnica.com
// on 2026-08-31, `tomekeeper/1.0.1 (+url)` was refused at the edge with a bare
// 403 while `Mozilla/5.0 (compatible; SomeBot/1.0; +url)` was served, so the
// filter rejects honesty rather than rejecting bots. The remedy is the
// long-standing `Mozilla/5.0 (compatible; name/version; +url)` convention that
// Googlebot and bingbot use, where the Mozilla token is vestigial and the
// parenthetical still names the crawler and how to reach its operator.
//
// What must not happen here is a UA that claims to be a person at a browser.
// The rule stays contactable-and-honest; the concession is only to the costume
// the filter insists on, and only for the domains an operator has said it for.
func (c *Client) SetHostUserAgent(host, ua string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ua == "" {
		delete(c.userAgents, host)
		return
	}
	c.userAgents[host] = ua
}

// userAgentFor is the identity to send to a host: its override, or the default.
func (c *Client) userAgentFor(host string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ua, ok := c.userAgents[host]; ok {
		return ua
	}
	return c.userAgent
}

// Get issues a GET request subject to robots.txt, rate limiting, and retries.
func (c *Client) Get(ctx context.Context, rawURL string, header http.Header) (*http.Response, error) {
	return c.Do(ctx, Request{URL: rawURL, Header: header})
}

// Permit clears one request through the politeness rules without making it.
//
// This exists for exactly one caller: the headless renderer, which fetches a page
// through a browser rather than through this client. That page still has to obey
// robots.txt and still has to wait its turn behind this host's rate limit, and the
// alternative — a second implementation of both rules living next to the browser —
// is how the two come to disagree about a site that asked not to be crawled.
//
// It blocks until the host's rate limit allows a request, then returns nil if the
// path is permitted. A disallowed path returns ErrDisallowedByRobots, exactly as Do
// would, so the caller's handling of a site that said no is identical either way.
//
// The global in-flight cap is deliberately not taken here. It bounds requests this
// process is making, and the requests a render causes are made by the browser; the
// render queue's own narrow concurrency is what bounds those.
func (c *Client) Permit(ctx context.Context, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parsing URL %q: %w", rawURL, err)
	}
	if parsed.Host == "" {
		return fmt.Errorf("URL %q has no host", rawURL)
	}

	allowed, err := c.robotsAllows(ctx, parsed)
	if err != nil {
		// Permissive on an unreadable robots.txt, the same deviation Do makes and for
		// the same reason: a host briefly failing to serve the file has not asked for
		// anything.
		allowed = true
	}
	if !allowed {
		return fmt.Errorf("%s: %w", rawURL, ErrDisallowedByRobots)
	}

	return c.waitForHost(ctx, parsed.Host)
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
		// Recorded per attempt rather than per call, because the interesting
		// number for a site that is rate-limiting is how many 429s it sent, not
		// how many of our calls eventually succeeded anyway.
		observe(host, resp, err)
		if err != nil {
			lastErr = err
			// Not transient, and not the network's doing: the destination is inside
			// this machine's own neighborhood and will still be in twenty minutes.
			// Returned at once for the same reason robots.txt is — three attempts at
			// a refusal is three times the log lines for one answer, which is what
			// the live incident produced before this guard existed.
			if errors.Is(err, ErrPrivateAddress) {
				return nil, err
			}
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
	// Set last so a caller cannot accidentally override the honest identity. A
	// deliberate per-host override goes through SetHostUserAgent, which is
	// configuration rather than a header a caller happened to pass.
	httpReq.Header.Set("User-Agent", c.userAgentFor(httpReq.URL.Host))

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
	// Matched against the identity this host is actually sent, not the default:
	// a host with an override would otherwise be tested under a name it never
	// sees, and a site naming that name in robots.txt would go unhonored.
	return data.TestAgent(path, c.userAgentFor(target.Host)), nil
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

// observe records the outcome of one attempt for the metrics endpoint.
//
// Per attempt rather than per call, because the interesting number for a site that
// is rate-limiting is how many 429s it sent, not how many of our calls eventually
// succeeded anyway.
func observe(host string, resp *http.Response, err error) {
	switch {
	case err != nil:
		metrics.OutboundFailures.WithLabelValues(host).Inc()
	case resp != nil:
		metrics.OutboundResponses.WithLabelValues(host, statusClass(resp.StatusCode)).Inc()
	}
}

// statusClass buckets a status code.
//
// By class rather than exact code: the difference between 502 and 503 is not worth
// a time series per host, while the difference between 2xx and 4xx is the whole
// question. 429 keeps its own bucket, because it is the one status that means
// "you are being told to slow down", and averaging it into 4xx hides it.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code == 429:
		return "429"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	default:
		return "2xx"
	}
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
