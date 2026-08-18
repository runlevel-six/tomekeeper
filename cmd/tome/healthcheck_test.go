package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The healthcheck answers about a server, not about itself.
//
// The whole reason it exists is that a check which only proves the binary runs would
// report "healthy" for a server failing every request — and a status line that lies
// costs more than a missing one.
func TestHealthcheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		want    int
		mention string
	}{
		{"a healthy server", http.StatusOK, exitOK, ""},
		{"a server answering 503", http.StatusServiceUnavailable, exitFailure, "503"},
		{"a server answering 500", http.StatusInternalServerError, exitFailure, "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			var stdout, stderr bytes.Buffer
			code := healthcheck([]string{"--addr", strings.TrimPrefix(srv.URL, "http://")}, &stdout, &stderr)

			if code != tc.want {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tc.want, stderr.String())
			}
			// Liveness, not readiness: /healthz answers without consulting the
			// database, so a database restart must not restart every container.
			if gotPath != "/healthz" {
				t.Errorf("checked %q, want /healthz", gotPath)
			}
			if tc.mention != "" && !strings.Contains(stderr.String(), tc.mention) {
				t.Errorf("stderr does not name the status %s:\n%s", tc.mention, stderr.String())
			}
		})
	}
}

// Nothing listening is unhealthy, and says where it looked.
func TestHealthcheckWithNothingListening(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// Port 1 is reserved and nothing will be on it.
	code := healthcheck([]string{"--addr", "127.0.0.1:1", "--timeout", "1s"}, &stdout, &stderr)

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "127.0.0.1:1") {
		t.Errorf("stderr does not say where it looked:\n%s", stderr.String())
	}
}

// The address comes from the same variable the server binds, so changing the port
// does not silently leave the healthcheck pointing at the old one.
func TestHealthcheckUsesTheConfiguredAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("TOME_HTTP_ADDR", strings.TrimPrefix(srv.URL, "http://"))

	var stdout, stderr bytes.Buffer
	if code := healthcheck(nil, &stdout, &stderr); code != exitOK {
		t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}

	// A bare ":8080" is what a server binds and not something a client can dial, so
	// the host has to be filled in rather than passed through.
	t.Setenv("TOME_HTTP_ADDR", ":1")
	stderr.Reset()
	if code := healthcheck([]string{"--timeout", "1s"}, &stdout, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want failure", code)
	}
	if !strings.Contains(stderr.String(), "127.0.0.1:1") {
		t.Errorf("a bare port was not resolved to a dialable address:\n%s", stderr.String())
	}
}

// It needs no database, because configuration that fails to validate is exactly when
// somebody wants to ask what the server is doing.
func TestHealthcheckNeedsNoDatabaseURL(t *testing.T) {
	t.Setenv("TOME_DATABASE_URL", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if code := healthcheck([]string{"--addr", strings.TrimPrefix(srv.URL, "http://")}, &stdout, &stderr); code != exitOK {
		t.Errorf("exit code = %d with no database URL, want %d (stderr: %s)",
			code, exitOK, stderr.String())
	}
}
