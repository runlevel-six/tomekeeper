package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/auth"
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
