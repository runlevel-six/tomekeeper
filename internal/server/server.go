// Package server is the HTTP surface of `tome serve`.
//
// At M0 it serves only the health endpoints. The web UI (M4) and Fever API
// (M5) mount onto the same mux.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/config"
)

// Check is a named readiness probe for one dependency. A nil error means the
// dependency is usable right now.
//
// M0 registers none: there is nothing to depend on yet. M1 registers the
// database, M3 the blob root. See readyz in health.go for what that means.
type Check struct {
	Name string
	Func func(context.Context) error
}

// Server wraps an http.Server with this application's routes, timeouts, and
// shutdown behavior.
type Server struct {
	log    *slog.Logger
	cfg    *config.Config
	checks []Check
	http   *http.Server
}

// New builds a server. It does not listen; call Run.
func New(cfg *config.Config, log *slog.Logger, checks ...Check) *Server {
	s := &Server{log: log, cfg: cfg, checks: checks}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

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

// Handler exposes the routed handler for tests that do not want a live socket.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run listens and serves until ctx is canceled, then shuts down gracefully
// within the configured timeout. It returns nil on a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
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
