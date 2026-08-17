package session_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/session"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

func newCookieStore(t *testing.T, secret string, ttl time.Duration) *session.Cookie {
	t.Helper()

	c, err := session.NewCookie([]byte(secret), ttl, true)
	if err != nil {
		t.Fatalf("NewCookie() = %v", err)
	}
	return c
}

// issue runs Issue and returns a request carrying the resulting cookie, which is
// what every test here needs and what a browser would do.
func issue(t *testing.T, c *session.Cookie, userID store.UserID) *http.Request {
	t.Helper()

	rec := httptest.NewRecorder()
	if err := c.Issue(rec, userID); err != nil {
		t.Fatalf("Issue() = %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	for _, ck := range rec.Result().Cookies() {
		req.AddCookie(ck)
	}
	return req
}

func TestIssueThenIdentify(t *testing.T) {
	c := newCookieStore(t, "a reasonably long random secret", session.DefaultTTL)

	got, ok := c.Identify(issue(t, c, 42))
	if !ok {
		t.Fatal("Identify() = false for a session just issued")
	}
	if got != 42 {
		t.Errorf("Identify() = %d, want 42", got)
	}
}

func TestNoCookieIsNotIdentified(t *testing.T) {
	c := newCookieStore(t, "secret", session.DefaultTTL)

	if _, ok := c.Identify(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)); ok {
		t.Error("Identify() = true with no cookie present")
	}
}

// The cookie attributes are the security properties, so they are asserted rather
// than assumed. Losing HttpOnly to a careless edit would be invisible otherwise.
func TestCookieAttributes(t *testing.T) {
	c := newCookieStore(t, "secret", session.DefaultTTL)

	rec := httptest.NewRecorder()
	if err := c.Issue(rec, 1); err != nil {
		t.Fatalf("Issue() = %v", err)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Issue() set %d cookies, want 1", len(cookies))
	}
	ck := cookies[0]

	if ck.Name != session.CookieName {
		t.Errorf("name = %q, want %q", ck.Name, session.CookieName)
	}
	if !ck.HttpOnly {
		t.Error("HttpOnly is not set, so script on the page can read the session")
	}
	if !ck.Secure {
		t.Error("Secure is not set, so the session can travel over plain HTTP")
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax so cross-site POSTs do not carry it", ck.SameSite)
	}
	if ck.Path != "/" {
		t.Errorf("Path = %q, want /", ck.Path)
	}

	// The payload must not be readable. A signed-but-not-encrypted cookie would
	// show the user id in plain base64.
	if strings.Contains(ck.Value, "1") && len(ck.Value) < 20 {
		t.Errorf("cookie value looks like plaintext: %q", ck.Value)
	}
}

// Secure has to be switchable, because a self-hosted deployment on a plain-HTTP
// LAN address would otherwise appear to log in and then silently not be logged
// in — the cookie is set and never sent back.
func TestSecureCanBeDisabled(t *testing.T) {
	c, err := session.NewCookie([]byte("secret"), session.DefaultTTL, false)
	if err != nil {
		t.Fatalf("NewCookie() = %v", err)
	}

	rec := httptest.NewRecorder()
	if err := c.Issue(rec, 1); err != nil {
		t.Fatalf("Issue() = %v", err)
	}
	if rec.Result().Cookies()[0].Secure {
		t.Error("Secure is set even though it was disabled")
	}
}

// AES-GCM authenticates, so any edit to the cookie must fail closed rather than
// decrypt to something plausible.
func TestTamperedCookieIsRejected(t *testing.T) {
	c := newCookieStore(t, "secret", session.DefaultTTL)

	rec := httptest.NewRecorder()
	if err := c.Issue(rec, 7); err != nil {
		t.Fatalf("Issue() = %v", err)
	}
	original := rec.Result().Cookies()[0].Value

	// Mutations are applied to the decoded bytes, not to the base64 text.
	//
	// Flipping a base64 character looks equivalent and is not: the final character
	// of this cookie carries two insignificant bits, and Go's decoder is lenient
	// about them, so several characters decode to identical bytes. A test that
	// edited the last character therefore passed most of the time and failed
	// roughly one run in sixteen — the cookie really was unchanged.
	mutate := func(at int) string {
		raw, err := base64.RawURLEncoding.DecodeString(original)
		if err != nil {
			t.Fatalf("decoding the cookie we just issued: %v", err)
		}
		if at < 0 {
			at += len(raw)
		}
		raw[at] ^= 0x01
		return base64.RawURLEncoding.EncodeToString(raw)
	}

	mutations := map[string]string{
		"flipped a ciphertext bit": mutate(-1),
		"flipped a nonce bit":      mutate(0),
		"flipped a payload bit":    mutate(len(original) / 3),
		"truncated":                original[:len(original)/2],
		"empty":                    "",
		"not base64":               "!!! not base64 !!!",
		"appended":                 original + "AAAA",
	}

	for name, value := range mutations {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			req.AddCookie(&http.Cookie{Name: session.CookieName, Value: value})

			if id, ok := c.Identify(req); ok {
				t.Errorf("Identify() accepted a %s cookie as user %d", name, id)
			}
		})
	}
}

// A cookie minted under a different key must not be accepted. This is what makes
// rotating the key a revocation, and what stops one deployment's cookie working
// against another.
func TestCookieFromAnotherKeyIsRejected(t *testing.T) {
	mine := newCookieStore(t, "my secret", session.DefaultTTL)
	theirs := newCookieStore(t, "their secret", session.DefaultTTL)

	if id, ok := mine.Identify(issue(t, theirs, 9)); ok {
		t.Errorf("Identify() accepted a cookie sealed with a different key, as user %d", id)
	}
}

// The client controls its own cookie jar, so expiry has to be enforced from the
// payload rather than trusted to the browser dropping the cookie.
func TestExpiredSessionIsRejected(t *testing.T) {
	// Issue with a TTL that has already elapsed by the time Identify runs.
	c, err := session.NewCookie([]byte("secret"), time.Nanosecond, true)
	if err != nil {
		t.Fatalf("NewCookie() = %v", err)
	}

	req := issue(t, c, 3)
	time.Sleep(2 * time.Millisecond)

	if id, ok := c.Identify(req); ok {
		t.Errorf("Identify() accepted an expired session as user %d", id)
	}
}

func TestClearRevokesTheCookie(t *testing.T) {
	c := newCookieStore(t, "secret", session.DefaultTTL)

	rec := httptest.NewRecorder()
	c.Clear(rec)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Clear() set %d cookies, want 1", len(cookies))
	}
	ck := cookies[0]

	if ck.Value != "" {
		t.Errorf("Clear() left a value in the cookie: %q", ck.Value)
	}
	if ck.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negative so the browser deletes it", ck.MaxAge)
	}

	// And the cleared cookie must not identify anyone if it is sent back.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.AddCookie(ck)
	if id, ok := c.Identify(req); ok {
		t.Errorf("Identify() accepted a cleared cookie as user %d", id)
	}
}

func TestIssueRefusesInvalidUsers(t *testing.T) {
	c := newCookieStore(t, "secret", session.DefaultTTL)

	for _, id := range []store.UserID{0, -1} {
		if err := c.Issue(httptest.NewRecorder(), id); err == nil {
			t.Errorf("Issue() accepted user id %d", id)
		}
	}
}

func TestNewCookieValidatesItsArguments(t *testing.T) {
	if _, err := session.NewCookie(nil, session.DefaultTTL, true); err == nil {
		t.Error("NewCookie() accepted an empty secret")
	}
	if _, err := session.NewCookie([]byte("secret"), 0, true); err == nil {
		t.Error("NewCookie() accepted a zero TTL")
	}
	if _, err := session.NewCookie([]byte("secret"), -time.Hour, true); err == nil {
		t.Error("NewCookie() accepted a negative TTL")
	}
}

func TestGenerateKeyIsRandomAndLongEnough(t *testing.T) {
	first, err := session.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() = %v", err)
	}
	second, err := session.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() = %v", err)
	}

	if len(first) != session.KeyLength {
		t.Errorf("GenerateKey() returned %d bytes, want %d", len(first), session.KeyLength)
	}
	if string(first) == string(second) {
		t.Error("GenerateKey() returned the same key twice")
	}
}

// Two sessions for the same user must not produce the same cookie, or the nonce
// is being reused — which for GCM is a serious failure, not a cosmetic one.
func TestNonceIsNotReused(t *testing.T) {
	c := newCookieStore(t, "secret", session.DefaultTTL)

	seen := make(map[string]bool, 32)
	for range 32 {
		rec := httptest.NewRecorder()
		if err := c.Issue(rec, 1); err != nil {
			t.Fatalf("Issue() = %v", err)
		}
		value := rec.Result().Cookies()[0].Value
		if seen[value] {
			t.Fatalf("Issue() produced a repeated cookie value, so the GCM nonce is being reused")
		}
		seen[value] = true
	}
}
