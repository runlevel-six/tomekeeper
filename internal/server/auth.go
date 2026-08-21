package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/runlevel-six/tomekeeper/internal/asseturl"
	"github.com/runlevel-six/tomekeeper/internal/auth"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// userKey is the request-context key carrying the signed-in user.
//
// An unexported type, so nothing outside this package can write it. A handler
// reached through requireUser can rely on it being present, which is the point:
// the alternative is every handler re-reading the session and one of them
// eventually forgetting to check the result.
type userKey struct{}

// signedInUser returns the user a request is authenticated as.
//
// It panics if there is none, which is deliberate. Every caller sits behind
// requireUser, so a missing user is a routing mistake rather than a runtime
// condition — and a panic in development is preferable to silently serving one
// user's archive as though it belonged to nobody.
func signedInUser(r *http.Request) store.UserID {
	id, ok := r.Context().Value(userKey{}).(store.UserID)
	if !ok {
		panic("server: handler requires a signed-in user but is not behind requireUser")
	}
	return id
}

// requireUser rejects requests without a valid session.
//
// Browsers get a redirect to the sign-in page; anything asking for JSON gets a
// 401, because redirecting an API client to an HTML form produces a 200 full of
// markup and a very confusing bug report.
func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := s.sessions.Identify(r)
		if !ok {
			if wantsJSON(r) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, userID)))
	}
}

// requireUserOrSignature admits a request carrying either a valid session or a
// valid signature from this service.
//
// One route uses this: archived images. The web reader reaches them with a session
// and always has; a Fever client renders an article body in its own view with no
// cookie to offer, so the URLs in that body carry a signature instead. See
// internal/asseturl for what the signature covers and why the key comes from the
// session secret.
//
// The order matters and is deliberate: the signature is checked first, so an image
// fetch never touches the session store, and a request whose signature has expired
// falls through to the session path rather than being refused outright. A reader with
// both simply gets served.
func (s *Server) requireUserOrSignature(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.assetURLs != nil {
			if s.assetURLs.Verify(r.URL.Path, r.URL.Query().Get(asseturl.SignatureParam)) {
				next(w, r)
				return
			}
		}
		s.requireUser(next)(w, r)
	}
}

// wantsJSON reports whether the caller would rather have a status code than a
// redirect to a sign-in form.
//
// Deliberately biased toward "this is a browser": an absent or wildcard Accept
// header, or anything mentioning HTML, gets the redirect. Only a caller that
// asked for something specific and non-HTML gets the 401.
func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	for _, html := range []string{"text/html", "application/xhtml+xml", "*/*"} {
		if strings.Contains(accept, html) {
			return false
		}
	}
	return true
}

// loginPage is the template data for the sign-in form.
type loginPage struct {
	// User is nil on this page, which is what keeps the signed-in chrome out of
	// the base template.
	User       any
	Username   string
	Error      string
	NoPassword bool

	// Theme is always empty here and exists only because base.html reads it.
	//
	// Nobody is signed in, so there is no reader whose palette this could be —
	// and looking one up before authentication would leak whether an account
	// exists. It is a field rather than a template guard because base.html is
	// shared, and a missing field is an execution error that turns the sign-in
	// page into a 500 with no clue as to why.
	Theme string

	// TextScale is empty here for the same reasons, and this field is the proof
	// that the comment above was worth writing: adding the size preference to
	// base.html turned every unauthenticated page into a 500 until it existed.
	// The tests caught it; a reader locked out of their own archive would have
	// been the alternative.
	TextScale string

	// Unread is likewise always zero, and here for the same reason: base.html
	// puts the count in the page title, and an unread tally is exactly the sort
	// of thing a sign-in page must not be able to tell an anonymous visitor.
	Unread int64
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	// Already signed in: send them where they were going rather than presenting a
	// form they do not need.
	if _, ok := s.sessions.Identify(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	_, hash, err := s.store.System().Credentials(r.Context(), s.cfg.Username)
	noPassword := err == nil && hash == ""
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("reading credentials for the sign-in page", "error", err)
	}

	s.render(w, http.StatusOK, "login", loginPage{
		Username:   s.cfg.Username,
		NoPassword: noPassword || errors.Is(err, pgx.ErrNoRows),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, http.StatusBadRequest, "login", loginPage{Error: "That form could not be read."})
		return
	}

	username := r.PostFormValue("username")
	password := r.PostFormValue("password")

	// One message for every failure mode below. Which of "no such user", "no
	// password set", or "wrong password" applies is diagnostic information for the
	// operator's log, not for whoever is typing into the form.
	const failed = "Those credentials were not accepted."

	userID, hash, err := s.store.System().Credentials(r.Context(), username)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		s.log.Warn("sign-in attempt for an unknown user", "username", username)
		s.render(w, http.StatusUnauthorized, "login", loginPage{Username: username, Error: failed})
		return
	case err != nil:
		s.log.Error("reading credentials failed", "error", err)
		s.render(w, http.StatusInternalServerError, "login",
			loginPage{Username: username, Error: "Something went wrong. The log will say what."})
		return
	case hash == "":
		// Distinct log line, because the fix is an operator action and the
		// symptom otherwise looks exactly like a forgotten password.
		s.log.Warn("sign-in attempt but no password is set; run `tome migrate` with TOME_PASSWORD",
			"username", username)
		s.render(w, http.StatusUnauthorized, "login", loginPage{Username: username, NoPassword: true})
		return
	}

	ok, err := auth.Verify(hash, password)
	if err != nil {
		// A malformed hash is a broken row, not a wrong password, and saying so
		// separately is the difference between "check your typing" and "look at
		// the users table".
		s.log.Error("the stored password hash could not be read", "username", username, "error", err)
		s.render(w, http.StatusInternalServerError, "login",
			loginPage{Username: username, Error: "The stored password could not be read. The log will say what."})
		return
	}
	if !ok {
		s.log.Warn("failed sign-in", "username", username)
		s.render(w, http.StatusUnauthorized, "login", loginPage{Username: username, Error: failed})
		return
	}

	if err := s.sessions.Issue(w, userID); err != nil {
		s.log.Error("issuing a session failed", "error", err)
		s.render(w, http.StatusInternalServerError, "login",
			loginPage{Username: username, Error: "Signing in failed. The log will say what."})
		return
	}

	s.log.Info("signed in", "username", username, "user_id", userID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Clear(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
