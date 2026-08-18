// Package server is the HTTP surface of `tome serve`.
//
// Health endpoints, the web interface, and — later — the Fever API all mount
// onto the same mux.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/config"
	"github.com/runlevel-six/tomekeeper/internal/session"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// Check is a named readiness probe for one dependency. A nil error means the
// dependency is usable right now.
//
// `tome serve` registers the database. See readyz in health.go for what a
// failing check does and, just as importantly, what it does not do.
type Check struct {
	Name string
	Func func(context.Context) error
}

// Deps are the collaborators the web interface needs.
//
// Separate from Config because these are wired objects rather than settings, and
// a zero Deps is meaningful: it yields a health-only server, which is what the
// health tests exercise without needing a database.
type Deps struct {
	// Store is the data layer. Nil mounts no web interface.
	Store *store.Store

	// Sessions issues and reads the sign-in credential.
	Sessions session.Store

	// Search backs the search page. Nil falls back to Store's own
	// implementation, which is what production uses; the field exists so a test
	// or a future engine can substitute one.
	Search store.SearchIndex

	// Blobs serves archived images to the reader. Nil means images 404 — the
	// pages still work, which is the right failure for a misconfigured blob root
	// rather than refusing to start.
	Blobs blob.Store
}

// Server wraps an http.Server with this application's routes, timeouts, and
// shutdown behavior.
type Server struct {
	log      *slog.Logger
	cfg      *config.Config
	checks   []Check
	http     *http.Server
	store    *store.Store
	sessions session.Store
	search   store.SearchIndex
	blobs    blob.Store
	ui       *ui
}

// New builds a server. It does not listen; call Run.
//
// The web interface is mounted only when deps carries what it needs. A template
// that fails to parse is logged and the interface is left unmounted rather than
// panicking: the health endpoints are how an orchestrator finds out the process is
// alive, and taking them down because a page is malformed turns a rendering bug
// into a crash loop.
func New(cfg *config.Config, log *slog.Logger, deps Deps, checks ...Check) *Server {
	s := &Server{
		log: log, cfg: cfg, checks: checks,
		store: deps.Store, sessions: deps.Sessions,
		search: deps.Search, blobs: deps.Blobs,
	}
	if s.search == nil && s.store != nil {
		s.search = s.store.Search()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	if s.store != nil && s.sessions != nil {
		u, err := newUI()
		switch {
		case err != nil:
			s.log.Error("the web interface could not be built; serving health endpoints only", "error", err)
		default:
			s.ui = u
			s.mountWeb(mux)
		}
	}

	s.http = &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: s.withRequestLogging(mux),

		// Timeouts are set explicitly because Go's defaults are "none", and a
		// service that talks to the open internet with no read timeout is a
		// service one slow client can pin open indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB

		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

// mountWeb registers the web interface.
//
// Grouped in one place so that "which routes require a session" is answerable by
// reading nine lines rather than by auditing every handler. Everything except the
// sign-in pages and the stylesheet goes through requireUser.
func (s *Server) mountWeb(mux *http.ServeMux) {
	mux.HandleFunc("GET /static/", s.handleStatic)

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// Reading views. Every one of these goes through requireUser; that is the
	// whole reason they are listed together.
	mux.HandleFunc("GET /{$}", s.requireUser(s.handleStream))
	mux.HandleFunc("GET /all", s.requireUser(s.handleAll))
	mux.HandleFunc("GET /starred", s.requireUser(s.handleStarred))
	mux.HandleFunc("GET /saved", s.requireUser(s.handleSaved))
	mux.HandleFunc("POST /save", s.requireUser(s.handleSave))
	mux.HandleFunc("GET /search", s.requireUser(s.handleSearch))
	mux.HandleFunc("GET /settings", s.requireUser(s.handleSettings))
	mux.HandleFunc("POST /settings", s.requireUser(s.handleSaveSettings))
	mux.HandleFunc("GET /feeds", s.requireUser(s.handleFeeds))
	// Registered before the {id} pattern for readability only: they differ by
	// method, so ServeMux never has to choose between them.
	mux.HandleFunc("POST /feeds/import", s.requireUser(s.handleImportOPML))
	mux.HandleFunc("GET /feeds/{id}", s.requireUser(s.handleFeedStream))
	mux.HandleFunc("GET /tags/{id}", s.requireUser(s.handleTagStream))
	mux.HandleFunc("GET /attention", s.requireUser(s.handleAttention))
	mux.HandleFunc("GET /articles/{id}", s.requireUser(s.handleArticle))

	mux.HandleFunc("POST /articles/{id}/read", s.requireUser(s.handleToggleRead))
	mux.HandleFunc("POST /articles/{id}/star", s.requireUser(s.handleToggleStar))
	mux.HandleFunc("POST /articles/{id}/keep", s.requireUser(s.handleToggleKept))

	// Archived images. Behind requireUser like everything else: the archive is
	// one person's reading history, and its illustrations are part of it.
	mux.HandleFunc("GET /assets/", s.requireUser(s.handleAsset))
}

// Handler exposes the routed handler for tests that do not want a live socket.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run listens and serves until ctx is canceled, then shuts down gracefully
// within the configured timeout. It returns nil on a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	// ListenConfig rather than net.Listen so that a shutdown signal arriving
	// while the socket is still being bound cancels the bind instead of being
	// noticed only afterwards.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.http.Addr)
	if err != nil {
		// A cancellation arriving while the socket is still being bound is a
		// shutdown, not a failure: the caller asked to stop before serving
		// began, and this function's contract is to return nil on a clean
		// shutdown. A genuine bind failure — an occupied port — still surfaces.
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	s.log.Info("http server listening", "addr", ln.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		// Serve always returns a non-nil error; ErrServerClosed is the normal
		// consequence of Shutdown and is not a failure.
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout)

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		// Deadline exceeded here means in-flight requests were dropped. Say so
		// rather than exiting 0 and pretending it was clean.
		s.log.Error("graceful shutdown did not complete", "error", err)
		return err
	}

	<-errCh
	s.log.Info("shutdown complete")
	return nil
}

// statusRecorder captures the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// withRequestLogging emits one structured line per request.
//
// Health checks log at debug: a kubelet probing /healthz every few seconds
// would otherwise be the overwhelming majority of the log volume, and drown
// the lines that matter.
func (s *Server) withRequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		switch {
		case isProbe(r.URL.Path):
			level = slog.LevelDebug
		case rec.status >= 500:
			level = slog.LevelError
		}

		s.log.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start),
			"remote", r.RemoteAddr,
		)
	})
}

func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz"
}
