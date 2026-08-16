package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/config"
	"github.com/runlevel-six/tomekeeper/internal/server"
)

func testConfig() *config.Config {
	cfg, err := config.Load(func(k string) (string, bool) {
		if k == "TOME_DATABASE_URL" {
			return "postgres://tome@db:5432/tome", true
		}
		return "", false
	})
	if err != nil {
		panic(err)
	}
	return cfg
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func get(t *testing.T, h http.Handler, path string) (*http.Response, map[string]any) {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	res := rec.Result()
	t.Cleanup(func() { _ = res.Body.Close() })

	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("GET %s: decoding body: %v", path, err)
	}
	return res, body
}

// The M0 acceptance criterion, at the handler level. The container-level
// version of the same assertion lives in scripts/smoke.sh.
func TestHealthzIsAlwaysOK(t *testing.T) {
	srv := server.New(testConfig(), discardLogger())

	res, body := get(t, srv.Handler(), "/healthz")

	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if got, want := body["status"], "ok"; got != want {
		t.Errorf("status field = %v, want %v", got, want)
	}
	if got, want := res.Header.Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
}

// Liveness must not depend on any registered dependency: a failing database
// should take this instance out of the load balancer, not get it killed.
func TestHealthzIgnoresFailingChecks(t *testing.T) {
	srv := server.New(testConfig(), discardLogger(), server.Check{
		Name: "database",
		Func: func(context.Context) error { return errors.New("connection refused") },
	})

	res, _ := get(t, srv.Handler(), "/healthz")

	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d with a failing check, want %d", res.StatusCode, http.StatusOK)
	}
}

func TestReadyzWithNoChecks(t *testing.T) {
	srv := server.New(testConfig(), discardLogger())

	res, body := get(t, srv.Handler(), "/readyz")

	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /readyz status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if got, want := body["status"], "ready"; got != want {
		t.Errorf("status field = %v, want %v", got, want)
	}
	if _, present := body["checks"]; present {
		t.Errorf("body has a checks field with no checks registered: %v", body)
	}
}

func TestReadyzWithPassingChecks(t *testing.T) {
	srv := server.New(testConfig(), discardLogger(), server.Check{
		Name: "database",
		Func: func(context.Context) error { return nil },
	})

	res, body := get(t, srv.Handler(), "/readyz")

	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /readyz status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	checks, _ := body["checks"].(map[string]any)
	if got, want := checks["database"], "ok"; got != want {
		t.Errorf("checks.database = %v, want %v", got, want)
	}
}

func TestReadyzWithFailingCheck(t *testing.T) {
	srv := server.New(testConfig(), discardLogger(),
		server.Check{Name: "healthy", Func: func(context.Context) error { return nil }},
		server.Check{Name: "database", Func: func(context.Context) error {
			return errors.New("connection refused")
		}},
	)

	res, body := get(t, srv.Handler(), "/readyz")

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz status = %d, want %d", res.StatusCode, http.StatusServiceUnavailable)
	}
	if got, want := body["status"], "not ready"; got != want {
		t.Errorf("status field = %v, want %v", got, want)
	}

	checks, _ := body["checks"].(map[string]any)
	if got, want := checks["database"], "connection refused"; got != want {
		t.Errorf("checks.database = %v, want %v", got, want)
	}
	// A failing dependency must not hide the state of the healthy ones.
	if got, want := checks["healthy"], "ok"; got != want {
		t.Errorf("checks.healthy = %v, want %v", got, want)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	srv := server.New(testConfig(), discardLogger())

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestProbesRejectNonGET(t *testing.T) {
	srv := server.New(testConfig(), discardLogger())

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// Run must serve on a real socket and return cleanly when its context is
// canceled — this is what makes a rolling restart graceful rather than a drop.
func TestRunServesAndShutsDownCleanly(t *testing.T) {
	cfg := testConfig()
	cfg.HTTPAddr = "127.0.0.1:0" // let the kernel pick a free port
	cfg.ShutdownTimeout = 5 * time.Second

	srv := server.New(cfg, discardLogger())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v, want nil on canceled context", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return within 10s of context cancellation")
	}
}

// A port already in use must fail loudly at startup rather than logging and
// carrying on with no listener.
func TestRunFailsOnUnavailablePort(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })

	cfg := testConfig()
	cfg.HTTPAddr = occupied.Addr().String()

	srv := server.New(cfg, discardLogger())

	if err := srv.Run(t.Context()); err == nil {
		t.Errorf("Run() = nil, want an error binding to the occupied address %s", cfg.HTTPAddr)
	}
}
