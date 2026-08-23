package server

import (
	"bytes"
	"compress/gzip"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/auth"
	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/config"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/session"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// In-package because requireUser and requireAdmin are unexported and there is no
// route wired to the admin check yet. Testing the middleware directly is also the
// more durable shape: what is being proved is the gate, not any page behind it.

// accessFixture is a Server with just enough wired to run the middleware, plus
// the store and cookie store the test needs to change the world underneath it.
type accessFixture struct {
	srv      *Server
	store    *store.Store
	sessions *session.Cookie
	userID   store.UserID
}

func newAccessFixture(t *testing.T) accessFixture {
	t.Helper()

	_, s, userID := dbtest.SetupWithUser(t)

	sessions, err := session.NewCookie([]byte("a test session secret"), session.DefaultTTL, true)
	if err != nil {
		t.Fatalf("NewCookie() = %v", err)
	}

	return accessFixture{
		srv: &Server{
			log:      slog.New(slog.DiscardHandler),
			cfg:      &config.Config{},
			store:    s,
			sessions: sessions,
		},
		store:    s,
		sessions: sessions,
		userID:   userID,
	}
}

// request returns a request carrying a credential sealed with the given identity,
// which is what a browser holding that cookie would send.
func (f accessFixture) request(t *testing.T, id session.Identity) *http.Request {
	t.Helper()

	rec := httptest.NewRecorder()
	if err := f.sessions.Issue(rec, id); err != nil {
		t.Fatalf("Issue() = %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	for _, ck := range rec.Result().Cookies() {
		req.AddCookie(ck)
	}
	return req
}

// currentIdentity is what this user's browser would be holding right now.
func (f accessFixture) currentIdentity(t *testing.T) session.Identity {
	t.Helper()

	account, err := f.store.System().SessionUser(t.Context(), f.userID)
	if err != nil {
		t.Fatalf("SessionUser() = %v", err)
	}
	return session.Identity{UserID: account.ID, Epoch: account.SessionEpoch}
}

// reached records whether the wrapped handler ran, which is the only thing any of
// these tests actually want to know.
func reached(hit *bool) http.HandlerFunc {
	return func(_ http.ResponseWriter, _ *http.Request) { *hit = true }
}

// The ordinary case. Without this the tests below could all pass by refusing
// everybody, which is a gate nobody would notice was broken until they were
// locked out of their own archive.
func TestAValidSessionReachesTheHandler(t *testing.T) {
	f := newAccessFixture(t)

	var hit bool
	rec := httptest.NewRecorder()
	f.srv.requireUser(reached(&hit))(rec, f.request(t, f.currentIdentity(t)))

	if !hit {
		t.Fatalf("a current session was refused with status %d", rec.Code)
	}
}

// The hole this slice exists to close: a credential is a claim about a moment
// that has passed, and the account may be gone by the time it is presented.
func TestASessionForADeletedUserIsRefused(t *testing.T) {
	f := newAccessFixture(t)
	identity := f.currentIdentity(t)

	if _, err := f.store.Pool().Exec(t.Context(), `DELETE FROM users WHERE id = $1`, f.userID); err != nil {
		t.Fatalf("deleting the user: %v", err)
	}

	var hit bool
	rec := httptest.NewRecorder()
	f.srv.requireUser(reached(&hit))(rec, f.request(t, identity))

	if hit {
		t.Error("a session for a deleted account reached the handler")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want %d (a browser is sent to sign in)", rec.Code, http.StatusSeeOther)
	}

	// The cookie must be cleared as well as refused. Left in place, the browser
	// presents the same dead credential on every request afterwards, and the
	// reader gets a sign-in page they cannot get past by signing in.
	if !clearsSession(rec) {
		t.Error("the dead credential was refused but left in the browser")
	}
}

// The epoch is the revocation mechanism, so a credential from before a bump must
// stop working — that is what makes a password change or a sign-out-everywhere
// mean anything.
func TestASessionFromBeforeARevocationIsRefused(t *testing.T) {
	f := newAccessFixture(t)
	stale := f.currentIdentity(t)

	epoch, err := f.store.System().BumpSessionEpoch(t.Context(), f.userID)
	if err != nil {
		t.Fatalf("BumpSessionEpoch() = %v", err)
	}
	if epoch == stale.Epoch {
		t.Fatalf("BumpSessionEpoch() left the epoch at %d; the test would prove nothing", epoch)
	}

	var hit bool
	rec := httptest.NewRecorder()
	f.srv.requireUser(reached(&hit))(rec, f.request(t, stale))

	if hit {
		t.Error("a session issued before the revocation reached the handler")
	}
	if !clearsSession(rec) {
		t.Error("the revoked credential was refused but left in the browser")
	}

	// And a credential issued after the bump works, or "revocation" would just be
	// a broken login.
	var afterHit bool
	after := httptest.NewRecorder()
	f.srv.requireUser(reached(&afterHit))(after, f.request(t, f.currentIdentity(t)))
	if !afterHit {
		t.Errorf("a session issued after the revocation was also refused, status %d", after.Code)
	}
}

func TestAnAdminReachesAnAdminHandler(t *testing.T) {
	f := newAccessFixture(t)

	// The seeded account is the operator and is seeded admin; asserted here
	// because every other case in this file depends on it.
	if account, err := f.store.System().SessionUser(t.Context(), f.userID); err != nil {
		t.Fatalf("SessionUser() = %v", err)
	} else if !account.IsAdmin() {
		t.Fatalf("the seeded user has role %q, want %q", account.Role, store.RoleAdmin)
	}

	var hit bool
	rec := httptest.NewRecorder()
	f.srv.requireAdmin(reached(&hit))(rec, f.request(t, f.currentIdentity(t)))

	if !hit {
		t.Fatalf("an admin was refused an admin handler, status %d", rec.Code)
	}
}

// 404 rather than 403, so the response does not confirm that the route exists —
// the same reasoning that makes another reader's article not-found.
func TestAReaderIsNotToldAnAdminPageExists(t *testing.T) {
	f := newAccessFixture(t)

	if _, err := f.store.Pool().Exec(t.Context(),
		`UPDATE users SET role = $2 WHERE id = $1`, f.userID, store.RoleReader); err != nil {
		t.Fatalf("demoting the user: %v", err)
	}

	var hit bool
	rec := httptest.NewRecorder()
	f.srv.requireAdmin(reached(&hit))(rec, f.request(t, f.currentIdentity(t)))

	if hit {
		t.Error("a reader reached an admin handler")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d — a 403 would confirm the page is there",
			rec.Code, http.StatusNotFound)
	}
}

// requireAdmin wraps requireUser rather than sitting beside it, so an
// unauthenticated request must never reach the role check.
func TestAnAnonymousRequestNeverReachesTheRoleCheck(t *testing.T) {
	f := newAccessFixture(t)

	var hit bool
	rec := httptest.NewRecorder()
	f.srv.requireAdmin(reached(&hit))(rec,
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	if hit {
		t.Error("an anonymous request reached an admin handler")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
}

// clearsSession reports whether the response tells the browser to drop the
// session cookie.
func clearsSession(rec *httptest.ResponseRecorder) bool {
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == session.CookieName && ck.MaxAge < 0 {
			return true
		}
	}
	return false
}

// Signing out everywhere has to end the session making the request too, or the
// control does not mean what it says on the device the reader is holding.
func TestSigningOutEverywhereEndsThisSessionToo(t *testing.T) {
	f := newAccessFixture(t)
	before := f.currentIdentity(t)

	rec := httptest.NewRecorder()
	req := f.request(t, before)
	f.srv.requireUser(f.srv.handleSignOutEverywhere)(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (a redirect to sign in again)", rec.Code, http.StatusSeeOther)
	}
	if !clearsSession(rec) {
		t.Error("this browser was left holding a credential that will now be refused")
	}

	// The stored epoch moved, which is what ends the sessions on other devices —
	// the cleared cookie above only affects this one.
	after := f.currentIdentity(t)
	if after.Epoch == before.Epoch {
		t.Errorf("session_epoch is still %d, so other devices stay signed in", after.Epoch)
	}

	// And the credential this request arrived with no longer admits anybody.
	var hit bool
	replay := httptest.NewRecorder()
	f.srv.requireUser(reached(&hit))(replay, f.request(t, before))
	if hit {
		t.Error("the credential from before the sign-out still reaches handlers")
	}
}

// Setting a password revokes existing sessions, because a password change that
// left a signed-in browser alone would be no change at all to whoever has one.
func TestSettingAPasswordRevokesSessions(t *testing.T) {
	f := newAccessFixture(t)
	before := f.currentIdentity(t)

	p := auth.DefaultParams()
	p.Memory, p.Iterations = 8*1024, 1
	hash, err := auth.HashWith(p, "a new password")
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	if err := f.store.System().SetPassword(t.Context(), f.userID, hash,
		auth.FeverAPIKey("tome", "a new password")); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}

	var hit bool
	rec := httptest.NewRecorder()
	f.srv.requireUser(reached(&hit))(rec, f.request(t, before))

	if hit {
		t.Error("a session issued before the password changed still reaches handlers")
	}
}

// handler runs a request through the real mux, which is what proves a route is
// actually behind the middleware it is supposed to be behind. The middleware
// tests above prove the gate; these prove it was applied.
func (f accessFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	return New(&config.Config{}, slog.New(slog.DiscardHandler),
		Deps{Store: f.store, Sessions: f.sessions}).Handler()
}

func (f accessFixture) get(t *testing.T, h http.Handler, path string, id session.Identity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	if err := f.sessions.Issue(rec, id); err != nil {
		t.Fatalf("Issue() = %v", err)
	}
	for _, ck := range rec.Result().Cookies() {
		req.AddCookie(ck)
	}
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	return out
}

// Every account route has to be behind requireAdmin. Listed rather than tested
// one by one so that adding a route without gating it fails here.
func TestAccountRoutesAreAdminOnly(t *testing.T) {
	f := newAccessFixture(t)
	h := f.handler(t)

	if _, err := f.store.Pool().Exec(t.Context(),
		`UPDATE users SET role = $2 WHERE id = $1`, f.userID, store.RoleReader); err != nil {
		t.Fatalf("demoting the user: %v", err)
	}
	reader := f.currentIdentity(t)

	routes := []struct{ method, path string }{
		{http.MethodGet, "/users"},
		{http.MethodPost, "/users"},
		{http.MethodPost, "/users/1/link"},
		{http.MethodPost, "/users/1/delete"},
	}
	for _, route := range routes {
		req := httptest.NewRequestWithContext(t.Context(), route.method, route.path, nil)
		rec := httptest.NewRecorder()
		if err := f.sessions.Issue(rec, reader); err != nil {
			t.Fatalf("Issue() = %v", err)
		}
		for _, ck := range rec.Result().Cookies() {
			req.AddCookie(ck)
		}
		out := httptest.NewRecorder()
		h.ServeHTTP(out, req)

		if out.Code != http.StatusNotFound {
			t.Errorf("%s %s as a reader = %d, want %d",
				route.method, route.path, out.Code, http.StatusNotFound)
		}
	}
}

// The setup-password page is the one route that must work with no session at all,
// so it is worth proving it is not behind the gate by accident.
func TestTheSetupPageNeedsNoSession(t *testing.T) {
	f := newAccessFixture(t)
	h := f.handler(t)

	id, err := f.store.System().CreateUser(t.Context(), "jane", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}
	link, err := f.store.System().IssueSetupLink(t.Context(), id)
	if err != nil {
		t.Fatalf("IssueSetupLink() = %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/set-password?token="+link.Token, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET the setup page with no session = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); !strings.Contains(body, "jane") {
		t.Error("the page does not name the account it sets a password for")
	}
}

// A bad token must not distinguish itself from a spent one, and must not offer a
// form that cannot work.
func TestTheSetupPageRefusesAnUnusableLink(t *testing.T) {
	f := newAccessFixture(t)
	h := f.handler(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/set-password?token=nonsense", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if strings.Contains(rec.Body.String(), `type="password"`) {
		t.Error("a password form is offered for a link that cannot be redeemed")
	}
}

// A typo in the confirmation must not cost the invitation: the link is checked
// before it is spent, and only spent once the password is going to be stored.
func TestAMistypedConfirmationDoesNotSpendTheLink(t *testing.T) {
	f := newAccessFixture(t)
	h := f.handler(t)

	id, err := f.store.System().CreateUser(t.Context(), "jane", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}
	link, err := f.store.System().IssueSetupLink(t.Context(), id)
	if err != nil {
		t.Fatalf("IssueSetupLink() = %v", err)
	}

	post := func(password, again string) *httptest.ResponseRecorder {
		form := url.Values{
			"token":          {link.Token},
			"password":       {password},
			"password_again": {again},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/set-password",
			strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("a long enough password", "a different one"); rec.Code != http.StatusBadRequest {
		t.Errorf("mismatched confirmation = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	// Short is refused too, and also without spending the link.
	if rec := post("short", "short"); rec.Code != http.StatusBadRequest {
		t.Errorf("a short password = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	// The link still works, which is the whole point of checking before spending.
	if rec := post("a long enough password", "a long enough password"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the corrected attempt = %d, want %d — the link was spent by a typo",
			rec.Code, http.StatusSeeOther)
	}

	// And the password that was set is the one typed.
	account, err := f.store.System().Credentials(t.Context(), "jane")
	if err != nil {
		t.Fatalf("Credentials() = %v", err)
	}
	if ok, err := auth.Verify(account.PasswordHash, "a long enough password"); err != nil || !ok {
		t.Errorf("the stored password does not verify: %v, %v", ok, err)
	}
}

// Changing your own password signs out your other devices and keeps this one —
// otherwise the act of securing your account throws you out of it.
func TestChangingYourPasswordKeepsThisSessionAndEndsTheOthers(t *testing.T) {
	f := newAccessFixture(t)
	h := f.handler(t)

	p := auth.DefaultParams()
	p.Memory, p.Iterations = 8*1024, 1
	hash, err := auth.HashWith(p, "the current password")
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	if err := f.store.System().SetPassword(t.Context(), f.userID, hash,
		auth.FeverAPIKey("tome", "the current password")); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}

	// Two devices, both signed in with the credential as it stands now.
	elsewhere := f.currentIdentity(t)

	form := url.Values{
		"current_password":   {"the current password"},
		"new_password":       {"a brand new password"},
		"new_password_again": {"a brand new password"},
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/settings/password",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	issued := httptest.NewRecorder()
	if err := f.sessions.Issue(issued, elsewhere); err != nil {
		t.Fatalf("Issue() = %v", err)
	}
	for _, ck := range issued.Result().Cookies() {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("changing a password = %d, want %d", rec.Code, http.StatusOK)
	}

	// The other device is out.
	if out := f.get(t, h, "/settings", elsewhere); out.Code != http.StatusSeeOther {
		t.Errorf("the other device = %d, want %d (signed out)", out.Code, http.StatusSeeOther)
	}

	// This one is not: the response carried a fresh credential.
	var reissued *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == session.CookieName && ck.MaxAge >= 0 {
			reissued = ck
		}
	}
	if reissued == nil {
		t.Fatal("no fresh session was issued, so changing a password signs you out of your own browser")
	}
	follow := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/settings", nil)
	follow.AddCookie(reissued)
	after := httptest.NewRecorder()
	h.ServeHTTP(after, follow)
	if after.Code != http.StatusOK {
		t.Errorf("this browser after the change = %d, want %d", after.Code, http.StatusOK)
	}
}

// The current password is required, or an unattended signed-in browser is enough
// to lock its owner out of their archive.
func TestChangingAPasswordNeedsTheCurrentOne(t *testing.T) {
	f := newAccessFixture(t)
	h := f.handler(t)

	p := auth.DefaultParams()
	p.Memory, p.Iterations = 8*1024, 1
	hash, err := auth.HashWith(p, "the current password")
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	if err := f.store.System().SetPassword(t.Context(), f.userID, hash,
		auth.FeverAPIKey("tome", "the current password")); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}

	form := url.Values{
		"current_password":   {"not the current password"},
		"new_password":       {"a brand new password"},
		"new_password_again": {"a brand new password"},
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/settings/password",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	issued := httptest.NewRecorder()
	if err := f.sessions.Issue(issued, f.currentIdentity(t)); err != nil {
		t.Fatalf("Issue() = %v", err)
	}
	for _, ck := range issued.Result().Cookies() {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// And the old password still works, so nothing was changed on the way.
	account, err := f.store.System().Credentials(t.Context(), "tome")
	if err != nil {
		t.Fatalf("Credentials() = %v", err)
	}
	if ok, _ := auth.Verify(account.PasswordHash, "the current password"); !ok {
		t.Error("the password changed despite the current one being wrong")
	}
}

// Renaming rewrites the Fever key, because that key is derived from the name.
//
// The consequence if it did not: every mobile client would keep authenticating
// against a stored key that no longer corresponds to anything the reader can type,
// and nothing would say so. The key cannot be recomputed from the argon2 hash, which
// is why the form asks for the password.
func TestRenamingRewritesTheFeverKey(t *testing.T) {
	f := newAccessFixture(t)
	h := f.handler(t)

	const password = "the current password"
	p := auth.DefaultParams()
	p.Memory, p.Iterations = 8*1024, 1
	hash, err := auth.HashWith(p, password)
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	if err := f.store.System().SetPassword(t.Context(), f.userID, hash,
		auth.FeverAPIKey("tome", password)); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}

	rec := f.post(t, h, "/settings/username", url.Values{
		"username": {"renamed"},
		"password": {password},
	}, f.currentIdentity(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("rename = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	var stored string
	if err := f.store.Pool().QueryRow(t.Context(),
		`SELECT api_key FROM users WHERE id = $1`, f.userID).Scan(&stored); err != nil {
		t.Fatalf("reading the api key: %v", err)
	}
	if want := auth.FeverAPIKey("renamed", password); stored != want {
		t.Error("the Fever key was not rewritten for the new name; every mobile client " +
			"would now be authenticating against a key nobody can compute")
	}

	// And the rename actually happened.
	account, err := f.store.System().SessionUser(t.Context(), f.userID)
	if err != nil {
		t.Fatalf("SessionUser() = %v", err)
	}
	if account.Username != "renamed" {
		t.Errorf("username = %q, want the new one", account.Username)
	}
}

// A rename needs the password, or an unattended browser could change how somebody
// signs in.
func TestRenamingNeedsThePassword(t *testing.T) {
	f := newAccessFixture(t)
	h := f.handler(t)

	p := auth.DefaultParams()
	p.Memory, p.Iterations = 8*1024, 1
	hash, err := auth.HashWith(p, "the current password")
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	if err := f.store.System().SetPassword(t.Context(), f.userID, hash,
		auth.FeverAPIKey("tome", "the current password")); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}

	rec := f.post(t, h, "/settings/username", url.Values{
		"username": {"renamed"},
		"password": {"not it"},
	}, f.currentIdentity(t))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("rename with a wrong password = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	account, err := f.store.System().SessionUser(t.Context(), f.userID)
	if err != nil {
		t.Fatalf("SessionUser() = %v", err)
	}
	if account.Username != "tome" {
		t.Errorf("username = %q; it changed despite the wrong password", account.Username)
	}
}

// A reader may delete their own account, and the last administrator may not — an
// archive with none cannot make another.
func TestDeletingYourOwnAccount(t *testing.T) {
	f := newAccessFixture(t)
	h := f.handler(t)

	const password = "the current password"
	p := auth.DefaultParams()
	p.Memory, p.Iterations = 8*1024, 1
	hash, err := auth.HashWith(p, password)
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	if err := f.store.System().SetPassword(t.Context(), f.userID, hash,
		auth.FeverAPIKey("tome", password)); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}

	// The seeded account is the only administrator, so it may not leave.
	rec := f.post(t, h, "/settings/delete-account",
		url.Values{"password": {password}}, f.currentIdentity(t))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("the last admin deleting themselves = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if _, err := f.store.System().SessionUser(t.Context(), f.userID); err != nil {
		t.Fatalf("the last administrator was deleted anyway: %v", err)
	}

	// With somebody else to administer it, leaving is allowed.
	if _, err := f.store.System().CreateUser(t.Context(), "other-admin", store.RoleAdmin); err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}
	rec = f.post(t, h, "/settings/delete-account",
		url.Values{"password": {password}}, f.currentIdentity(t))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("deleting my own account = %d, want %d\n%s",
			rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if !clearsSession(rec) {
		t.Error("the session was left in the browser after the account was deleted")
	}
	if _, err := f.store.System().SessionUser(t.Context(), f.userID); err == nil {
		t.Error("the account still exists after deleting it")
	}
}

// Deleting your own account needs your password: a signed-in browser somebody
// walked away from must not be enough to destroy their reading.
func TestDeletingYourOwnAccountNeedsThePassword(t *testing.T) {
	f := newAccessFixture(t)
	h := f.handler(t)

	p := auth.DefaultParams()
	p.Memory, p.Iterations = 8*1024, 1
	hash, err := auth.HashWith(p, "the current password")
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	if err := f.store.System().SetPassword(t.Context(), f.userID, hash,
		auth.FeverAPIKey("tome", "the current password")); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}
	if _, err := f.store.System().CreateUser(t.Context(), "other-admin", store.RoleAdmin); err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}

	rec := f.post(t, h, "/settings/delete-account",
		url.Values{"password": {"not it"}}, f.currentIdentity(t))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if _, err := f.store.System().SessionUser(t.Context(), f.userID); err != nil {
		t.Error("the account was deleted despite the wrong password")
	}
}

// post sends a form as a signed-in reader.
func (f accessFixture) post(
	t *testing.T, h http.Handler, path string, form url.Values, id session.Identity,
) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path,
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	issued := httptest.NewRecorder()
	if err := f.sessions.Issue(issued, id); err != nil {
		t.Fatalf("Issue() = %v", err)
	}
	for _, ck := range issued.Result().Cookies() {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The explanation is reachable from a browser, which is the whole point: the
// command it replaces needed a terminal and, on Kubernetes, permission to exec into
// a pod — neither of which the reader who most needs it is likely to have.
func TestExplainIsReachableAndScoped(t *testing.T) {
	f := newAccessFixture(t)

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}
	page := `<!DOCTYPE html><html><head><title>Explained</title></head><body>` +
		`<article class="main"><p>` + strings.Repeat("Words about alpacas. ", 40) + `</p></article>` +
		`</body></html>`
	const path = "articles/test/explained/raw.html.gz"
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(page)); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := blobs.Put(t.Context(), path, bytes.NewReader(gz.Bytes())); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	h := New(&config.Config{}, slog.New(slog.DiscardHandler),
		Deps{Store: f.store, Sessions: f.sessions, Blobs: blobs}).Handler()

	const url = "https://explained.example/story"
	articleID, _, err := f.store.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: url, URLOriginal: url, Title: "Explained",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if err := f.store.RecordFetchSuccess(t.Context(), articleID,
		store.FetchedPage{SHA: "sha-explained", Path: path}); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}

	// Not visible to this reader yet: an article they may not see must be
	// not-found rather than explained, or the explanation describes a page they
	// are not entitled to know exists.
	hidden := f.get(t, h, "/articles/"+strconv.FormatInt(int64(articleID), 10)+"/explain",
		f.currentIdentity(t))
	if hidden.Code != http.StatusNotFound {
		t.Errorf("explaining an invisible article = %d, want %d", hidden.Code, http.StatusNotFound)
	}

	// Now visible, through a subscription.
	feedID, _, err := f.store.UpsertFeed(t.Context(), f.userID, store.FeedParams{
		FeedURL: "https://explained.example/feed.xml", Title: "Explained",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if _, err := f.store.InsertFeedItem(t.Context(), f.userID, store.FeedItemParams{
		FeedID: feedID, ArticleID: articleID, GUID: "g", Title: "Explained",
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}

	rec := f.get(t, h, "/articles/"+strconv.FormatInt(int64(articleID), 10)+"/explain",
		f.currentIdentity(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("explain = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"What each step produced", "trafilatura", "chosen"} {
		if !strings.Contains(body, want) {
			t.Errorf("the explanation does not mention %q", want)
		}
	}
	// It says which rules applied, which is the first thing somebody debugging
	// their own selector needs.
	if !strings.Contains(body, "The rules that apply to you here") {
		t.Error("the explanation does not say which rules applied")
	}

	// And it describes *this reader's* extraction, not the archive's.
	//
	// With no rule of their own the two are identical, so the page could explain
	// the household's and nobody would notice — which is exactly what neutering
	// found. A rule of their own is what makes the difference observable, and it is
	// also the case the page exists for: somebody debugging a selector they wrote.
	if err := f.store.System().UpsertReaderRule(t.Context(), f.userID, store.DomainRule{
		Domain: "explained.example", ContentSelector: "article.main",
	}); err != nil {
		t.Fatalf("UpsertReaderRule() = %v", err)
	}

	mine := f.get(t, h, "/articles/"+strconv.FormatInt(int64(articleID), 10)+"/explain",
		f.currentIdentity(t))
	if mine.Code != http.StatusOK {
		t.Fatalf("explain after writing a rule = %d, want 200", mine.Code)
	}
	switch body := mine.Body.String(); {
	case !strings.Contains(body, "article.main"):
		t.Error("the explanation does not show the reader's own selector")
	case !strings.Contains(body, "yours"):
		t.Error("the explanation does not say the rule is the reader's own")
	}
}
