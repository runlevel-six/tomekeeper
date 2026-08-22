package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/runlevel-six/tomekeeper/internal/auth"
	"github.com/runlevel-six/tomekeeper/internal/session"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// usersPage is the account list an administrator manages.
type usersPage struct {
	pageData

	Accounts []store.AccountSummary
	Roles    []struct{ Value, Name, Blurb string }

	// Notice reports what just happened, and Problem why something did not.
	Notice  string
	Problem string

	// Issued is a link that has just been created. It is shown once, on this
	// render, and never again: the database keeps only a hash, so there is no
	// page that can show it a second time.
	Issued    string
	IssuedFor string
	IssuedTil time.Time

	// Confirm is the account a deletion is being asked about, or nil.
	Confirm *store.AccountSummary
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	s.renderUsers(w, r, http.StatusOK, usersPage{})
}

// handleCreateUser adds an account with no password.
//
// It does not issue a link in the same step, deliberately. Creating an account and
// handing somebody a credential are separate decisions, and folding them together
// would mean every mistyped username produced a live link too.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	username := r.PostFormValue("username")
	// Assembled from the known roles rather than taken from the form, like the
	// theme picker: a hand-crafted POST cannot invent a privilege level.
	role := store.RoleReader
	if r.PostFormValue("role") == store.RoleAdmin {
		role = store.RoleAdmin
	}

	id, err := s.store.System().CreateUser(r.Context(), username, role)
	switch {
	case errors.Is(err, store.ErrUsernameBlank):
		s.renderUsers(w, r, http.StatusBadRequest, usersPage{Problem: "A name is needed to create an account."})
		return
	case errors.Is(err, store.ErrUsernameTaken):
		// Named rather than generic, the same way a colliding feed address is: the
		// reader needs to know which name to change.
		s.renderUsers(w, r, http.StatusConflict, usersPage{
			Problem: "There is already an account called " + username + "."})
		return
	case errors.Is(err, store.ErrUsernameInvalid):
		s.renderUsers(w, r, http.StatusBadRequest, usersPage{
			Problem: "A username cannot contain spaces or control characters, and must be short enough to type."})
		return
	case err != nil:
		s.log.Error("creating an account failed", "error", err)
		s.renderUsers(w, r, http.StatusInternalServerError, usersPage{
			Problem: "That account could not be created. The log will say why."})
		return
	}

	s.log.Info("account created", "user_id", id, "role", role, "by", signedInUser(r))
	s.renderUsers(w, r, http.StatusOK, usersPage{
		Notice: "Created " + username + ". They cannot sign in until a password is set — " +
			"issue a link below and hand it over."})
}

// handleIssueSetupLink creates a single-use link and shows it once.
func (s *Server) handleIssueSetupLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}

	account, err := s.store.System().SessionUser(r.Context(), store.UserID(id))
	if err != nil {
		s.notFoundOrError(w, r, err, "looking up an account")
		return
	}

	link, err := s.store.System().IssueSetupLink(r.Context(), store.UserID(id))
	if err != nil {
		s.log.Error("issuing a setup link failed", "user_id", id, "error", err)
		s.renderUsers(w, r, http.StatusInternalServerError, usersPage{
			Problem: "That link could not be issued. The log will say why."})
		return
	}

	s.log.Info("setup link issued", "user_id", id, "by", signedInUser(r))
	s.renderUsers(w, r, http.StatusOK, usersPage{
		Issued:    "/set-password?token=" + link.Token,
		IssuedFor: account.Username,
		IssuedTil: link.ExpiresAt,
	})
}

// handleDeleteUser asks first, then removes an account.
//
// GET with ?delete=<id> asks; POST acts. The same two-step shape as unsubscribe
// and the bulk mark, and for the same reason: there is no unsafe-inline in the
// content security policy, so a confirmation is a page rather than a dialog.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}

	if store.UserID(id) == signedInUser(r) {
		// Refused rather than allowed, because the administrator doing it is the
		// one who would then be signed out mid-action with no way back if they
		// were also the last admin. The store's guard would catch that case; this
		// one keeps the interface from offering a control that deletes the person
		// pressing it.
		s.renderUsers(w, r, http.StatusBadRequest, usersPage{
			Problem: "You cannot delete the account you are signed in as."})
		return
	}

	err := s.store.System().DeleteUser(r.Context(), store.UserID(id))
	switch {
	case errors.Is(err, store.ErrLastAdmin):
		s.renderUsers(w, r, http.StatusBadRequest, usersPage{
			Problem: "That is the only administrator. An archive without one cannot make another here."})
		return
	case errors.Is(err, pgx.ErrNoRows):
		http.NotFound(w, r)
		return
	case err != nil:
		s.log.Error("deleting an account failed", "user_id", id, "error", err)
		s.renderUsers(w, r, http.StatusInternalServerError, usersPage{
			Problem: "That account could not be deleted. The log will say why."})
		return
	}

	s.log.Info("account deleted", "user_id", id, "by", signedInUser(r))
	s.renderUsers(w, r, http.StatusOK, usersPage{
		Notice: "That account is gone, with its subscriptions, tags and reading state. " +
			"Every article and image is kept."})
}

func (s *Server) renderUsers(w http.ResponseWriter, r *http.Request, status int, page usersPage) {
	page.pageData = s.pageData(r, "users")
	page.Roles = []struct{ Value, Name, Blurb string }{
		{store.RoleReader, "Reader", "Their own feeds, tags and reading"},
		{store.RoleAdmin, "Administrator", "Also domain rules, retention and accounts"},
	}

	accounts, err := s.store.System().ListAccounts(r.Context())
	if err != nil {
		s.log.Error("listing accounts failed", "error", err)
		if page.Problem == "" {
			page.Problem = "The accounts could not be listed. The log will say why."
		}
	}
	page.Accounts = accounts

	// The delete confirmation is asked for by query parameter and answered from
	// the list already loaded, so a stale id simply asks about nothing.
	if raw := r.URL.Query().Get("delete"); raw != "" {
		for i := range accounts {
			if idString(accounts[i].ID) == raw {
				page.Confirm = &accounts[i]
				break
			}
		}
	}

	s.render(w, status, "users", page)
}

// handleChangePassword changes the signed-in reader's own password.
//
// The current password is required. Without it, anybody who found an unattended
// signed-in browser could lock the owner out of their own archive — and the
// session cookie alone is not evidence that the person at the keyboard knows the
// password.
//
// A fresh session is issued afterwards. Changing a password revokes every session
// for that reader, which is the point, and would otherwise include the one making
// the request: the reader would change their password and be thrown out. Re-issuing
// here signs out the other devices and keeps this one.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	account := signedInAccount(r)
	current := r.PostFormValue("current_password")
	next := r.PostFormValue("new_password")
	again := r.PostFormValue("new_password_again")

	fail := func(status int, problem string) {
		s.renderSettings(w, r, status, false, problem)
	}

	// The stored hash is not in the account the middleware loaded — it is, but
	// reading it from there would make the session row a credential store. Load
	// it deliberately.
	stored, err := s.store.System().SessionUser(r.Context(), account.ID)
	if err != nil {
		s.log.Error("loading the account to change its password failed", "error", err)
		fail(http.StatusInternalServerError, "That could not be checked just now. The log will say why.")
		return
	}

	ok, err := auth.Verify(stored.PasswordHash, current)
	if err != nil {
		s.log.Error("the stored password hash could not be read", "user_id", account.ID, "error", err)
		fail(http.StatusInternalServerError, "The stored password could not be read. The log will say what.")
		return
	}
	if !ok {
		s.log.Warn("failed password change", "user_id", account.ID)
		fail(http.StatusUnauthorized, "That is not your current password, so nothing changed.")
		return
	}

	switch {
	case next == "":
		fail(http.StatusBadRequest, "A new password is needed.")
		return
	case next != again:
		fail(http.StatusBadRequest, "The two new passwords did not match, so nothing changed.")
		return
	case len(next) < auth.MinPasswordLength:
		fail(http.StatusBadRequest, auth.MinPasswordAdvice)
		return
	}

	hash, err := auth.Hash(next)
	if err != nil {
		s.log.Error("hashing a password failed", "error", err)
		fail(http.StatusInternalServerError, "That password could not be stored. The log will say why.")
		return
	}
	if err := s.store.System().SetPassword(r.Context(), account.ID, hash,
		auth.FeverAPIKey(account.Username, next)); err != nil {
		s.log.Error("changing a password failed", "user_id", account.ID, "error", err)
		fail(http.StatusInternalServerError, "That password could not be stored. The log will say why.")
		return
	}

	// Re-read rather than assuming the epoch moved by one: what has to be sealed
	// is whatever the row says now, and guessing would sign the reader out of the
	// session being issued.
	updated, err := s.store.System().SessionUser(r.Context(), account.ID)
	if err == nil {
		err = s.sessions.Issue(w, session.Identity{UserID: updated.ID, Epoch: updated.SessionEpoch})
	}
	if err != nil {
		// The password did change, so saying otherwise would be worse than this:
		// the reader is signed out and their new password works.
		s.log.Error("could not issue a session after a password change", "user_id", account.ID, "error", err)
		s.sessions.Clear(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	s.log.Info("password changed", "user_id", account.ID)
	s.renderSettings(w, r, http.StatusOK, true, "")
}

// setPasswordPage is the page a setup link leads to. It is served to nobody in
// particular — there is no session here — so it carries none of the signed-in
// chrome.
type setPasswordPage struct {
	User      any
	Theme     string
	TextScale string
	Unread    int64

	// IsAdmin is always false here and exists because base.html reads it. See
	// loginPage, which carries the same set of fields for the same reason.
	IsAdmin bool

	Token    string
	Username string
	Error    string

	// Usable is false when the link is spent, expired or unknown, in which case
	// the form is not offered at all.
	Usable bool
}

// handleSetPasswordForm shows the form a setup link leads to.
//
// Unauthenticated by necessity: the whole point is that the person opening it
// cannot sign in yet. The token in the query string is the only credential, which
// is why it is 256 bits of randomness and why the row it matches is single-use.
func (s *Server) handleSetPasswordForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")

	account, err := s.store.System().SetupLinkAccount(r.Context(), token)
	if err != nil {
		if !errors.Is(err, store.ErrLinkUnusable) {
			s.log.Error("looking up a setup link failed", "error", err)
		}
		s.render(w, http.StatusNotFound, "setpassword", setPasswordPage{})
		return
	}

	s.render(w, http.StatusOK, "setpassword", setPasswordPage{
		Token: token, Username: account.Username, Usable: true,
	})
}

// handleSetPassword spends the link and stores the chosen password.
func (s *Server) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	token := r.PostFormValue("token")
	password := r.PostFormValue("password")
	again := r.PostFormValue("password_again")

	// Checked before the link is spent, so a typo does not cost the invitation.
	// This is the reason redemption is not attempted first and validated after.
	account, err := s.store.System().SetupLinkAccount(r.Context(), token)
	if err != nil {
		if !errors.Is(err, store.ErrLinkUnusable) {
			s.log.Error("looking up a setup link failed", "error", err)
		}
		s.render(w, http.StatusNotFound, "setpassword", setPasswordPage{})
		return
	}

	page := setPasswordPage{Token: token, Username: account.Username, Usable: true}
	switch {
	case password == "":
		page.Error = "A password is needed."
		s.render(w, http.StatusBadRequest, "setpassword", page)
		return
	case password != again:
		page.Error = "Those two did not match."
		s.render(w, http.StatusBadRequest, "setpassword", page)
		return
	case len(password) < auth.MinPasswordLength:
		page.Error = auth.MinPasswordAdvice
		s.render(w, http.StatusBadRequest, "setpassword", page)
		return
	}

	hash, err := auth.Hash(password)
	if err != nil {
		s.log.Error("hashing a password failed", "error", err)
		page.Error = "That password could not be stored. The log will say why."
		s.render(w, http.StatusInternalServerError, "setpassword", page)
		return
	}

	// The link is spent here, inside the store's transaction, so two people
	// racing one link produce one password and one refusal.
	if _, err := s.store.System().RedeemSetupLink(r.Context(), token, hash,
		auth.FeverAPIKey(account.Username, password)); err != nil {
		if errors.Is(err, store.ErrLinkUnusable) {
			s.render(w, http.StatusNotFound, "setpassword", setPasswordPage{})
			return
		}
		s.log.Error("redeeming a setup link failed", "user_id", account.ID, "error", err)
		page.Error = "That password could not be stored. The log will say why."
		s.render(w, http.StatusInternalServerError, "setpassword", page)
		return
	}

	// Not signed in automatically. Signing in with the password just chosen is
	// the act that proves it was typed as intended, and this page has no session
	// to build on anyway.
	s.log.Info("password set from a setup link", "user_id", account.ID)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func idString(id store.UserID) string {
	return strconv.FormatInt(int64(id), 10)
}
