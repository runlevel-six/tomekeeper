package server_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/auth"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/server"
	"github.com/runlevel-six/tomekeeper/internal/session"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// These require a live PostgreSQL and skip without TOME_TEST_DATABASE_URL,
// because the sign-in path reads a real credential row.

const testPassword = "correct horse battery staple"

// signedInServer returns a server with the web interface mounted and a password
// set, plus the session store its cookies can be read with.
func signedInServer(t *testing.T, withPassword bool) (http.Handler, *session.Cookie) {
	t.Helper()

	_, s, userID := dbtest.SetupWithUser(t)

	if withPassword {
		// Cheap parameters: this is exercising the handler, not the KDF.
		p := auth.DefaultParams()
		p.Memory, p.Iterations = 8*1024, 1

		hash, err := auth.HashWith(p, testPassword)
		if err != nil {
			t.Fatalf("HashWith() = %v", err)
		}
		if err := s.System().SetPassword(t.Context(), userID, hash,
			auth.FeverAPIKey("tome", testPassword)); err != nil {
			t.Fatalf("SetPassword() = %v", err)
		}
	}

	sessions, err := session.NewCookie([]byte("a test session secret"), session.DefaultTTL, true)
	if err != nil {
		t.Fatalf("NewCookie() = %v", err)
	}

	srv := server.New(testConfig(), discardLogger(), server.Deps{Store: s, Sessions: sessions})
	return srv.Handler(), sessions
}

func postLogin(t *testing.T, h http.Handler, username, password string) *httptest.ResponseRecorder {
	t.Helper()

	form := url.Values{"username": {username}, "password": {password}}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSignInWithCorrectPassword(t *testing.T) {
	h, sessions := signedInServer(t, true)

	rec := postLogin(t, h, "tome", testPassword)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login = %d, want %d\n%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want /", got)
	}

	// The response must carry a session that identifies the seeded user.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	for _, ck := range rec.Result().Cookies() {
		req.AddCookie(ck)
	}
	id, ok := sessions.Identify(req)
	if !ok {
		t.Fatal("no usable session cookie was issued on a successful sign-in")
	}
	if id.UserID != store.SeedUserID {
		t.Errorf("session identifies user %d, want %d", id.UserID, store.SeedUserID)
	}
}

// Every rejection has to look the same to whoever is typing. Which of "no such
// user" or "wrong password" applies is for the operator's log, not the form.
func TestSignInRejectionsAreIndistinguishable(t *testing.T) {
	h, _ := signedInServer(t, true)

	wrongPassword := postLogin(t, h, "tome", "not the password")
	unknownUser := postLogin(t, h, "someone-else", testPassword)

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"wrong password": wrongPassword,
		"unknown user":   unknownUser,
	} {
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want %d", name, rec.Code, http.StatusUnauthorized)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: a cookie was set on a failed sign-in", name)
		}
	}

	// The message the server *chose* is what must not differ. Comparing whole
	// pages would fail on the echoed username, which is the attacker's own input
	// and no disclosure — and naively normalizing it out is worse, since the
	// default username is a substring of "/static/tome.css".
	wrongMsg := errorMessage(t, wrongPassword.Body.String())
	unknownMsg := errorMessage(t, unknownUser.Body.String())

	if wrongMsg != unknownMsg {
		t.Errorf("the two rejections say different things, which tells an attacker which usernames exist:\n"+
			"wrong password: %q\nunknown user:   %q", wrongMsg, unknownMsg)
	}
	if wrongMsg == "" {
		t.Error("a rejected sign-in showed no message at all")
	}

	// And neither may hint at the distinction in any other wording.
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"wrong password": wrongPassword,
		"unknown user":   unknownUser,
	} {
		lower := strings.ToLower(rec.Body.String())
		for _, leak := range []string{"no such user", "unknown user", "user not found", "no password"} {
			if strings.Contains(lower, leak) {
				t.Errorf("%s: the page contains %q, which distinguishes the failure modes", name, leak)
			}
		}
	}
}

// errorMessage pulls the text out of the page's error paragraph.
func errorMessage(t *testing.T, body string) string {
	t.Helper()

	m := errorParagraph.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

var errorParagraph = regexp.MustCompile(`(?s)<p class="error" role="alert">(.*?)</p>`)

// With no password set the page has to say so, because the symptom is otherwise
// identical to a forgotten password and the fix is an operator action.
func TestSignInBeforeAPasswordIsSet(t *testing.T) {
	h, _ := signedInServer(t, false)

	rec := postLogin(t, h, "tome", "anything")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if body := rec.Body.String(); !strings.Contains(body, "TOME_PASSWORD") {
		t.Errorf("the page does not name TOME_PASSWORD, so the reader is not told how to fix it:\n%s", body)
	}

	// And the form itself is not offered, since there is nothing to sign in with.
	form := httptest.NewRecorder()
	h.ServeHTTP(form, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/login", nil))
	if strings.Contains(form.Body.String(), `type="password"`) {
		t.Error("a password field is shown even though no password exists to enter")
	}
}

// The point of requireUser: an unauthenticated browser must never reach a page
// that would show the archive.
func TestProtectedPageRedirectsWhenSignedOut(t *testing.T) {
	h, _ := signedInServer(t, true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET / signed out = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/login" {
		t.Errorf("Location = %q, want /login", got)
	}
}

// A non-browser caller gets a status code rather than a redirect to a form, so an
// expired session does not arrive as a 200 full of HTML.
func TestProtectedPageReturns401ForNonBrowsers(t *testing.T) {
	h, _ := signedInServer(t, true)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Accept", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET / with Accept: application/json = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestProtectedPageServedWhenSignedIn(t *testing.T) {
	h, _ := signedInServer(t, true)

	login := postLogin(t, h, "tome", testPassword)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	for _, ck := range login.Result().Cookies() {
		req.AddCookie(ck)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / signed in = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Sign out") {
		t.Errorf("the signed-in chrome is missing from the page:\n%s", body)
	}
}

func TestSignOutRevokesTheSession(t *testing.T) {
	h, sessions := signedInServer(t, true)

	login := postLogin(t, h, "tome", testPassword)

	out := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/logout", nil)
	for _, ck := range login.Result().Cookies() {
		out.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, out)

	if rec.Code != http.StatusSeeOther {
		t.Errorf("POST /logout = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	// The cookie the browser is left holding must not identify anyone.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	for _, ck := range rec.Result().Cookies() {
		req.AddCookie(ck)
	}
	if id, ok := sessions.Identify(req); ok {
		t.Errorf("the cookie left after signing out still identifies user %d", id.UserID)
	}
}

// Visiting the form while already signed in should not present it again.
func TestLoginFormRedirectsWhenAlreadySignedIn(t *testing.T) {
	h, _ := signedInServer(t, true)

	login := postLogin(t, h, "tome", testPassword)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/login", nil)
	for _, ck := range login.Result().Cookies() {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Errorf("GET /login while signed in = %d, want a redirect", rec.Code)
	}
}

// The stylesheet is part of the binary and must be reachable without a session,
// or the sign-in page renders unstyled.
func TestStylesheetIsServedWithoutASession(t *testing.T) {
	h, _ := signedInServer(t, true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/static/tome.css", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/tome.css = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "css") {
		t.Errorf("Content-Type = %q, want it to mention css", ct)
	}
}

// The archive renders markup from arbitrary websites, so the response headers
// that constrain it are worth asserting rather than trusting.
func TestPagesCarryTheirSecurityHeaders(t *testing.T) {
	h, _ := signedInServer(t, true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/login", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy on an HTML page")
	}
	for _, want := range []string{"default-src 'none'", "form-action 'self'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP is missing %q: %s", want, csp)
		}
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}
