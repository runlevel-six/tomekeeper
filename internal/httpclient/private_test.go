package httpclient

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// The guard against fetching this machine's own neighborhood.
//
// Every case here runs against the client that runs in production rather than
// against the predicate alone, because the interesting question is not whether
// 127.0.0.1 is loopback — netip knows that — but whether a request can reach it
// anyway: through a redirect, through a name, or through a resolver answer that
// differs from whatever a URL check saw.

// The live incident, as a test: a site redirects a poll to loopback.
//
// thetruthaboutguns.com did exactly this on 2026-08-23, and the fetcher dialed it —
// five times over an hour and three quarters, with the backoff working perfectly
// around a destination it should never have tried.
//
// The site is reached by *name* here, allowed as `localhost`, and its redirect
// target is an address. That is what lets both live on loopback while only one of
// them is permitted, and it doubles as the assertion that a name allowance does not
// quietly extend to the addresses that name resolves to.
func TestRedirectToLoopbackIsRefused(t *testing.T) {
	var reached bool
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = io.WriteString(w, "the thing only this machine can see")
	}))
	defer internal.Close()

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer site.Close()

	allow, err := ParsePrivateAllowance("localhost")
	if err != nil {
		t.Fatalf("ParsePrivateAllowance() = %v", err)
	}
	c := New(Options{UserAgent: "tomekeeper/test", MaxAttempts: 1, DefaultRPS: 1000, AllowPrivate: allow})

	resp, err := c.Get(t.Context(), byName(t, site.URL)+"/article", nil)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the redirect to loopback was followed")
	}
	if !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("Get() = %v, want ErrPrivateAddress", err)
	}
	if reached {
		t.Error("the internal server was reached, which is the whole failure this prevents")
	}
	// The reason has to name the redirect. "connection refused against 127.0.0.1" is
	// what the incident looked like in the log, and it sent the diagnosis in the
	// wrong direction.
	if !strings.Contains(err.Error(), "redirected to") {
		t.Errorf("the error does not say a redirect caused it: %v", err)
	}
}

// A refusal is not retried, because it will not come good.
func TestRefusalIsNotRetried(t *testing.T) {
	var attempts int
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		attempts++
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer site.Close()

	c, slept := newTestClient(t, Options{MaxAttempts: 3, DefaultRPS: 1000})

	if _, err := c.Get(t.Context(), site.URL+"/article", nil); !errors.Is(err, ErrPrivateAddress) { //nolint:bodyclose // the call fails, so the response is nil
		t.Fatalf("Get() = %v, want ErrPrivateAddress", err)
	}
	if attempts != 1 {
		t.Errorf("the site saw %d requests, want 1: three attempts at a permanent refusal "+
			"is three times the log lines for one answer", attempts)
	}
	if len(*slept) != 0 {
		t.Errorf("backoff was slept between attempts of a permanent refusal: %v", *slept)
	}
}

// The addresses that matter, refused by the production client with no allowance.
func TestNonPublicAddressesAreRefused(t *testing.T) {
	for _, target := range []string{
		// The cloud metadata service: the single most valuable address an SSRF can
		// reach on a hosted machine.
		"http://169.254.169.254/latest/meta-data/",
		// This cluster's API server and the shape of any in-namespace Service.
		"http://10.43.0.1:6443/api/v1/secrets",
		"http://192.168.1.1/",
		"http://[fd00::1]/",
		// Carrier-grade NAT is other customers of the same ISP, not the internet.
		"http://100.64.0.1/",
		"http://0.0.0.0/",
		"http://127.0.0.1/",
	} {
		t.Run(target, func(t *testing.T) {
			c := New(Options{UserAgent: "tomekeeper/test", MaxAttempts: 1, DefaultRPS: 1000})

			_, err := c.Get(t.Context(), target, nil) //nolint:bodyclose // the call fails, so the response is nil
			if !errors.Is(err, ErrPrivateAddress) {
				t.Errorf("Get(%s) = %v, want ErrPrivateAddress", target, err)
			}
		})
	}
}

// The hook judges what the resolver returned, which is the case a URL check cannot
// see: the URL is honest, the name is public, and the address is internal. DNS
// rebinding is the same shape arriving one answer later.
func TestGuardJudgesResolvedAddresses(t *testing.T) {
	refused := []string{"127.0.0.1:80", "10.43.0.1:443", "169.254.169.254:80", "[::1]:8080",
		"[fe80::1]:80", "[64:ff9b::a2b:1]:80", "255.255.255.255:80"}
	for _, address := range refused {
		if err := guardAddress(PrivateAllowance{}, address); !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("guardAddress(%s) = %v, want ErrPrivateAddress", address, err)
		}
	}

	allowed := []string{"93.184.216.34:443", "[2606:2800:220:1:248:1893:25c8:1946]:443", "8.8.8.8:53"}
	for _, address := range allowed {
		if err := guardAddress(PrivateAllowance{}, address); err != nil {
			t.Errorf("guardAddress(%s) = %v, want a public address allowed", address, err)
		}
	}

	// An IPv4-mapped IPv6 address is the same destination wearing a different
	// spelling, and refusing one spelling only is not refusing it.
	if err := guardAddress(PrivateAllowance{}, "[::ffff:10.43.0.1]:80"); !errors.Is(err, ErrPrivateAddress) {
		t.Errorf("guardAddress(v4-mapped private) = %v, want ErrPrivateAddress", err)
	}
}

// The escape hatch opens what it names and nothing else.
func TestPrivateAllowance(t *testing.T) {
	allow, err := ParsePrivateAllowance(" 10.0.0.0/8 , 192.168.4.7, WIKI.lan ")
	if err != nil {
		t.Fatalf("ParsePrivateAllowance() = %v", err)
	}

	for _, address := range []string{"10.0.0.5:80", "10.255.255.255:443", "192.168.4.7:80"} {
		if err := guardAddress(allow, address); err != nil {
			t.Errorf("guardAddress(%s) = %v, want it allowed", address, err)
		}
	}
	// One address is not its range, another private range is not covered, and
	// loopback is not opened by opening a LAN.
	for _, address := range []string{"192.168.4.8:80", "172.16.0.1:80", "127.0.0.1:80", "169.254.169.254:80"} {
		if err := guardAddress(allow, address); !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("guardAddress(%s) = %v, want ErrPrivateAddress", address, err)
		}
	}

	// Names match case-insensitively and with a trailing dot, because both are the
	// same host and only one of them is what anybody would have typed.
	for _, host := range []string{"wiki.lan", "WIKI.LAN", "wiki.lan."} {
		if !allow.allowsHost(host) {
			t.Errorf("allowsHost(%q) = false, want the named host allowed", host)
		}
	}
	if allow.allowsHost("evil.wiki.lan") {
		t.Error("a subdomain of an allowed name is allowed, which the setting does not say")
	}
	if allow.Empty() {
		t.Error("Empty() is true for an allowance naming three things")
	}
}

// A misspelled network is refused at startup rather than silently never matching.
//
// That failure mode is the reason this is validated in config.Load: a setting that
// appears to be in force and matches nothing looks exactly like a guard that is
// broken, and the archive it cannot reach gives no hint which.
func TestPrivateAllowanceRejectsNonsense(t *testing.T) {
	for _, spec := range []string{"10.0.0.0/33", "10.0.0.0/8/8", "not a host", "192.168.1.1:80"} {
		if _, err := ParsePrivateAllowance(spec); err == nil {
			t.Errorf("ParsePrivateAllowance(%q) = nil, want a complaint", spec)
		}
	}

	allow, err := ParsePrivateAllowance(" , ,")
	if err != nil {
		t.Fatalf("ParsePrivateAllowance() = %v", err)
	}
	if !allow.Empty() {
		t.Errorf("an allowance of blanks is not empty: %s", allow)
	}
	if allow.allowsHost("") {
		t.Error("the empty host name is allowed")
	}
}

// A prefix is masked, so it means the network somebody plainly meant.
func TestPrivateAllowanceMasksPrefixes(t *testing.T) {
	allow, err := ParsePrivateAllowance("10.1.2.3/8")
	if err != nil {
		t.Fatalf("ParsePrivateAllowance() = %v", err)
	}
	if err := guardAddress(allow, "10.9.9.9:80"); err != nil {
		t.Errorf("guardAddress() = %v, want 10.1.2.3/8 to mean the whole 10/8", err)
	}
}

// A redirect to a scheme this archive does not fetch is refused by name.
func TestRedirectToOtherSchemeIsRefused(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Location", "file:///etc/passwd")
		w.WriteHeader(http.StatusFound)
	}))
	defer site.Close()

	c, _ := newTestClient(t, Options{MaxAttempts: 1, DefaultRPS: 1000})

	_, err := c.Get(t.Context(), site.URL+"/article", nil) //nolint:bodyclose // the call fails, so the response is nil
	if !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("Get() = %v, want ErrPrivateAddress", err)
	}
	if !strings.Contains(err.Error(), "file://") {
		t.Errorf("the error does not name the scheme it refused: %v", err)
	}
}

// A public address is still fetchable, which is the assertion that keeps the rest of
// these from passing because everything is refused.
func TestPublicPrefixesAreNotRefused(t *testing.T) {
	for _, addr := range []string{"93.184.216.34", "1.1.1.1", "2606:2800:220:1:248:1893:25c8:1946"} {
		if ok, why := publicAddr(netip.MustParseAddr(addr)); !ok {
			t.Errorf("publicAddr(%s) = false (%s), want a public address", addr, why)
		}
	}
}

// byName rewrites a test server's URL to reach it by name instead of by address.
func byName(t *testing.T, rawURL string) string {
	t.Helper()

	trimmed := strings.TrimPrefix(rawURL, "http://")
	_, port, err := net.SplitHostPort(trimmed)
	if err != nil {
		t.Fatalf("reading the test server's port from %q: %v", rawURL, err)
	}
	return "http://localhost:" + port
}
