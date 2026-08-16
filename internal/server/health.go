package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// checkTimeout bounds the whole readiness probe. A readiness endpoint that
// hangs is worse than one that reports failure: the orchestrator learns
// nothing and the probe eventually times out anyway.
const checkTimeout = 3 * time.Second

// handleHealthz is liveness. It answers "is this process running and able to
// serve HTTP" and nothing else.
//
// It deliberately does not touch the database. Liveness failure means the
// container gets killed, and killing every replica because Postgres is briefly
// unreachable turns a recoverable dependency outage into a crash loop. That
// distinction is what /readyz is for.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleReadyz is readiness. It answers "should this instance receive
// traffic", which does depend on every registered dependency.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()

	results := make(map[string]string, len(s.checks))
	ready := true

	for _, c := range s.checks {
		if err := c.Func(ctx); err != nil {
			results[c.Name] = err.Error()
			ready = false
			continue
		}
		results[c.Name] = "ok"
	}

	body := map[string]any{"status": "ready"}
	status := http.StatusOK
	if !ready {
		body["status"] = "not ready"
		status = http.StatusServiceUnavailable
	}
	if len(results) > 0 {
		body["checks"] = results
	}

	if !ready {
		s.log.Warn("readiness check failed", "checks", results)
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The body is a literal built above; an encode failure means the client
	// went away, which the access log already records.
	_ = json.NewEncoder(w).Encode(body)
}
