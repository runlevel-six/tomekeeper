package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient returns a client whose backoff is instant and deterministic,
// recording what it would have slept. Retry policy is about *how long* and
// *how many times*, and neither is worth waiting for in a test.
func newTestClient(t *testing.T, opts Options) (*Client, *[]time.Duration) {
	t.Helper()

	if opts.UserAgent == "" {
		opts.UserAgent = "tomekeeper/test"
	}
	// Every case here serves its fixtures from an httptest server on loopback, which
	// is the honest instance of the case TOME_FETCH_ALLOW_PRIVATE exists for: a
	// destination somebody deliberately pointed this client at. Said here rather than
	// exempted in the guard, so the guard that runs in these tests is the one that
	// runs in production. A case testing the refusal builds its client with New
	// directly, since the default here would otherwise hand it the exemption.
	if opts.AllowPrivate.Empty() {
		opts.AllowPrivate = LoopbackAllowance()
	}
	c := New(opts)

	var (
		mu    sync.Mutex
		slept []time.Duration
	)
	c.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		slept = append(slept, d)
		return nil
	}
	c.jitter = func(d time.Duration) time.Duration { return d }
	return c, &slept
}

func TestUserAgent(t *testing.T) {
	if got, want := UserAgent("1.2.3", ""), "tomekeeper/1.2.3"; got != want {
		t.Errorf("UserAgent() = %q, want %q", got, want)
	}
	got := UserAgent("1.2.3", "https://example.com/about")
	if want := "tomekeeper/1.2.3 (+https://example.com/about)"; got != want {
		t.Errorf("UserAgent() = %q, want %q", got, want)
	}
	if strings.Contains(strings.ToLower(got), "mozilla") {
		t.Error("the User-Agent impersonates a browser")
	}
}

// The politeness rules: retry 429 and 503 honoring Retry-After, never a 4xx other than 408
// and 429. A 404 will still be a 404 in ten seconds.
func TestRetryPolicy(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		retryAfter   string
		wantRequests int
	}{
		{"429 is retried", http.StatusTooManyRequests, "", 3},
		{"503 is retried", http.StatusServiceUnavailable, "", 3},
		{"500 is retried", http.StatusInternalServerError, "", 3},
		{"408 is retried", http.StatusRequestTimeout, "", 3},
		{"404 is not retried", http.StatusNotFound, "", 1},
		{"403 is not retried", http.StatusForbidden, "", 1},
		{"401 is not retried", http.StatusUnauthorized, "", 1},
		{"410 is not retried", http.StatusGone, "", 1},
		{"200 is not retried", http.StatusOK, "", 1},
		{"304 is not retried", http.StatusNotModified, "", 1},
		// A wait longer than a worker should be parked for is not slept
		// through; the response comes back and the job reschedules itself.
		{"long Retry-After is not slept through", http.StatusTooManyRequests, "600", 1},
		{"short Retry-After is honored", http.StatusTooManyRequests, "2", 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// robots.txt is a real request but not the one under test.
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				requests.Add(1)
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			c, _ := newTestClient(t, Options{MaxAttempts: 3})

			resp, err := c.Get(t.Context(), srv.URL, nil)
			if err != nil {
				t.Fatalf("Get() = %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if got := int(requests.Load()); got != tt.wantRequests {
				t.Errorf("server saw %d requests, want %d", got, tt.wantRequests)
			}
			if resp.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
			}
		})
	}
}

func TestRetryHonorsRetryAfterDuration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, slept := newTestClient(t, Options{MaxAttempts: 3})

	resp, err := c.Get(t.Context(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	for _, d := range *slept {
		if d != 3*time.Second {
			t.Errorf("waited %v between attempts, want the 3s the server asked for", d)
		}
	}
	if len(*slept) == 0 {
		t.Error("no wait was recorded between retries")
	}
}

// Without Retry-After, backoff is exponential rather than immediate.
func TestRetryBackoffIsExponential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, slept := newTestClient(t, Options{MaxAttempts: 4})

	resp, err := c.Get(t.Context(), srv.URL, nil)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(*slept) != len(want) {
		t.Fatalf("recorded %v waits, want %v", *slept, want)
	}
	for i, d := range *slept {
		if d != want[i] {
			t.Errorf("wait %d = %v, want %v", i, d, want[i])
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"120", 120 * time.Second, true},
		{"0", 0, true},
		{"", 0, false},
		{"-5", 0, false},
		{"soon", 0, false},
		// An HTTP-date in the past means "you may retry now".
		{"Mon, 17 Aug 2026 11:00:00 GMT", 0, true},
	}

	for _, tt := range tests {
		got, ok := parseRetryAfter(tt.value, now)
		if ok != tt.ok {
			t.Errorf("parseRetryAfter(%q) ok = %v, want %v", tt.value, ok, tt.ok)
			continue
		}
		if ok && tt.want != 0 && got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}

	// A future HTTP-date produces a positive wait.
	if got, ok := parseRetryAfter(time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat), time.Now()); !ok || got <= 0 {
		t.Errorf("parseRetryAfter(future date) = %v, %v; want a positive duration", got, ok)
	}
}

// robots.txt is respected for article fetches. This is the rule that keeps
// this service welcome, and the one whose absence gets addresses banned.
func TestRobotsDisallow(t *testing.T) {
	var articleRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = io.WriteString(w, "User-agent: *\nDisallow: /private/\n")
			return
		}
		articleRequests.Add(1)
		_, _ = io.WriteString(w, "the article")
	}))
	defer srv.Close()

	c, _ := newTestClient(t, Options{MaxAttempts: 1})

	if _, err := c.Get(t.Context(), srv.URL+"/private/secret", nil); !errors.Is(err, ErrDisallowedByRobots) { //nolint:bodyclose // asserts the call fails, so the discarded response is nil
		t.Errorf("Get() on a disallowed path = %v, want ErrDisallowedByRobots", err)
	}
	if got := articleRequests.Load(); got != 0 {
		t.Errorf("the disallowed path was fetched %d times, want 0", got)
	}

	resp, err := c.Get(t.Context(), srv.URL+"/public/post", nil)
	if err != nil {
		t.Fatalf("Get() on an allowed path = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := articleRequests.Load(); got != 1 {
		t.Errorf("the allowed path was fetched %d times, want 1", got)
	}
}

// Fetching robots.txt once per host, not once per article, is what keeps the
// check from doubling every fetch.
func TestRobotsIsCached(t *testing.T) {
	var robotsRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robotsRequests.Add(1)
			_, _ = io.WriteString(w, "User-agent: *\nAllow: /\n")
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c, _ := newTestClient(t, Options{MaxAttempts: 1, DefaultRPS: 1000})

	for range 4 {
		resp, err := c.Get(t.Context(), srv.URL+"/article", nil)
		if err != nil {
			t.Fatalf("Get() = %v", err)
		}
		_ = resp.Body.Close()
	}

	if got := robotsRequests.Load(); got != 1 {
		t.Errorf("robots.txt was fetched %d times for 4 articles, want 1", got)
	}
}

// A feed is a subscription the reader asked for, so it is exempt. This is a
// deliberate deviation from "robots.txt for everything" and is documented.
func TestRobotsSkippedForFeeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = io.WriteString(w, "User-agent: *\nDisallow: /\n")
			return
		}
		_, _ = io.WriteString(w, "<rss/>")
	}))
	defer srv.Close()

	c, _ := newTestClient(t, Options{MaxAttempts: 1})

	resp, err := c.Do(t.Context(), Request{URL: srv.URL + "/feed.xml", SkipRobots: true})
	if err != nil {
		t.Fatalf("Do() with SkipRobots = %v, want the feed to be polled", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The same path without the exemption is refused, proving the exemption is
	// what let it through.
	if _, err := c.Get(t.Context(), srv.URL+"/feed.xml", nil); !errors.Is(err, ErrDisallowedByRobots) { //nolint:bodyclose // asserts the call fails, so the discarded response is nil
		t.Errorf("Get() without SkipRobots = %v, want ErrDisallowedByRobots", err)
	}
}

// A robots.txt that cannot be fetched must not stop the archive. The site
// never actually said no.
func TestUnreachableRobotsIsPermissive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "the article")
	}))
	defer srv.Close()

	c, _ := newTestClient(t, Options{MaxAttempts: 1})

	resp, err := c.Get(t.Context(), srv.URL+"/article", nil)
	if err != nil {
		t.Fatalf("Get() = %v, want a 5xx robots.txt to be treated as permissive", err)
	}
	defer func() { _ = resp.Body.Close() }()
}

// The per-host limit is the one servers actually experience.
func TestPerHostRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	// 20 requests per second with a burst of 3: the fourth request onward has
	// to wait for a token, so six requests take at least 3 intervals.
	c, _ := newTestClient(t, Options{MaxAttempts: 1, DefaultRPS: 20})

	start := time.Now()
	for range 6 {
		resp, err := c.Get(t.Context(), srv.URL+"/a", nil)
		if err != nil {
			t.Fatalf("Get() = %v", err)
		}
		_ = resp.Body.Close()
	}
	elapsed := time.Since(start)

	// Burst of 3 immediately, then 3 more at 50ms apart, minus the robots
	// fetch which also consumes a token.
	if elapsed < 100*time.Millisecond {
		t.Errorf("6 requests at 20 rps took %v, want the limiter to have delayed them", elapsed)
	}
}

func TestSetHostRate(t *testing.T) {
	c := New(Options{UserAgent: "test", DefaultRPS: 1})

	c.SetHostRate("slow.example.com", 0.1)
	c.mu.Lock()
	limiter, ok := c.limiters["slow.example.com"]
	c.mu.Unlock()
	if !ok {
		t.Fatal("SetHostRate did not create a limiter")
	}
	if got, want := float64(limiter.Limit()), 0.1; got != want {
		t.Errorf("limit = %v, want %v", got, want)
	}

	// Updating an existing limiter must change it in place, so a domain rule
	// edit takes effect without a restart.
	c.SetHostRate("slow.example.com", 5)
	if got, want := float64(limiter.Limit()), 5.0; got != want {
		t.Errorf("limit after update = %v, want %v", got, want)
	}

	// A non-positive rate restores the default.
	c.SetHostRate("slow.example.com", 0)
	if got, want := float64(limiter.Limit()), 1.0; got != want {
		t.Errorf("limit after reset = %v, want the default %v", got, want)
	}
}

// The global cap bounds concurrent requests across every host, so the machine
// itself cannot become the burst.
func TestGlobalConcurrencyCap(t *testing.T) {
	var (
		inFlight atomic.Int32
		peak     atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		n := inFlight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	const cap = 2
	c, _ := newTestClient(t, Options{MaxAttempts: 1, Concurrency: cap, DefaultRPS: 1000})

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.Get(t.Context(), srv.URL+"/a", nil)
			if err != nil {
				return
			}
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > cap {
		t.Errorf("peak concurrent requests = %d, want no more than %d", got, cap)
	}
}

func TestReadBodyRejectsOversizedResponse(t *testing.T) {
	body := strings.NewReader(strings.Repeat("x", MaxResponseBytes+1))

	if _, err := ReadBody(body); err == nil {
		t.Error("ReadBody() = nil, want an error for a body over the limit")
	} else if !strings.Contains(err.Error(), "limit") {
		t.Errorf("ReadBody() error = %q, want it to mention the limit", err)
	}

	if _, err := ReadBody(strings.NewReader("small")); err != nil {
		t.Errorf("ReadBody() = %v for a small body", err)
	}
}

func TestRejectsURLWithoutHost(t *testing.T) {
	c, _ := newTestClient(t, Options{MaxAttempts: 1})

	if _, err := c.Get(t.Context(), "/relative/path", nil); err == nil { //nolint:bodyclose // asserts the call fails, so the discarded response is nil
		t.Error("Get() with a relative URL = nil, want an error")
	}
}

// A canceled context must abandon the request rather than finishing it.
func TestContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = io.WriteString(w, "too late")
	}))
	defer srv.Close()

	c, _ := newTestClient(t, Options{MaxAttempts: 1})

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if _, err := c.Do(ctx, Request{URL: srv.URL + "/slow", SkipRobots: true}); err == nil { //nolint:bodyclose // asserts the call fails, so the discarded response is nil
		t.Error("Do() = nil, want the canceled context to abandon the request")
	}
}

func TestSetHostUserAgent(t *testing.T) {
	c := New(Options{UserAgent: "tomekeeper/test", DefaultRPS: 1})

	if got, want := c.userAgentFor("plain.example.com"), "tomekeeper/test"; got != want {
		t.Errorf("userAgentFor(unoverridden host) = %q, want the default %q", got, want)
	}

	const disguised = "Mozilla/5.0 (compatible; tomekeeper/test; +https://example.com)"
	c.SetHostUserAgent("picky.example.com", disguised)

	if got := c.userAgentFor("picky.example.com"); got != disguised {
		t.Errorf("userAgentFor(overridden host) = %q, want %q", got, disguised)
	}
	// The override is per host and must not leak to any other.
	if got, want := c.userAgentFor("plain.example.com"), "tomekeeper/test"; got != want {
		t.Errorf("userAgentFor(other host) = %q, want the default %q", got, want)
	}

	// An empty string restores the default, so clearing the field in a domain
	// rule actually clears the override.
	c.SetHostUserAgent("picky.example.com", "")
	if got, want := c.userAgentFor("picky.example.com"), "tomekeeper/test"; got != want {
		t.Errorf("userAgentFor after reset = %q, want the default %q", got, want)
	}
}

// The override has to reach the wire, not just the lookup: the whole point is
// what the origin is told.
func TestPerHostUserAgentIsSent(t *testing.T) {
	const disguised = "Mozilla/5.0 (compatible; tomekeeper/test; +https://example.com)"

	var got struct {
		robots  string
		article string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			got.robots = r.UserAgent()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		got.article = r.UserAgent()
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, Options{MaxAttempts: 1, DefaultRPS: 100})

	host, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing the test server URL: %v", err)
	}
	c.SetHostUserAgent(host.Host, disguised)

	resp, err := c.Get(t.Context(), srv.URL+"/article", nil)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	_ = resp.Body.Close()

	if got.article != disguised {
		t.Errorf("article User-Agent = %q, want %q", got.article, disguised)
	}
	// robots.txt too. A host that filters on the User-Agent refuses that file
	// the same way it refuses an article, and being unable to read the rules is
	// how a fetcher ends up ignoring them.
	if got.robots != disguised {
		t.Errorf("robots.txt User-Agent = %q, want %q", got.robots, disguised)
	}
}
