// Package metrics exposes Prometheus metrics for the archive.
//
// It listens on its own address rather than joining the main HTTP mux, and that
// is a privacy decision rather than a stylistic one. The Ingress in front of this
// service routes `/`, so an endpoint on the main server would be reachable from
// the public internet — and metrics that name the hosts being fetched from are a
// published list of what someone reads. The separate listener is not routed by
// the Ingress, so only something inside the cluster can reach it.
//
// Most values are read from the database at scrape time instead of being counted
// in the application. The archive's interesting numbers are all already facts in
// Postgres — how many feeds are failing, how many articles have no body, how deep
// the queue is — so counting them again in memory would add a second source of
// truth that can drift, and would reset to zero on every restart. Scrapes are
// infrequent and the queries are cheap.
//
// What genuinely cannot be recovered from the database is instrumented directly:
// the outcome of outbound HTTP requests, which leaves no row behind when it
// succeeds.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Namespace prefixes every metric this application defines.
const Namespace = "tome"

// OutboundResponses counts HTTP responses from sites, by host and status.
//
// Instrumented rather than derived: a successful fetch updates an article row, but
// a 429 or a connection failure leaves nothing behind, and those are exactly the
// numbers worth watching. The host label is why this endpoint is not public.
var OutboundResponses = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "outbound_responses_total",
		Help:      "HTTP responses received from remote sites, by host and status class.",
	},
	[]string{"host", "status"},
)

// OutboundFailures counts requests that never produced a response at all.
var OutboundFailures = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "outbound_failures_total",
		Help:      "Outbound HTTP requests that failed before a response, by host.",
	},
	[]string{"host"},
)

// Registry holds this application's collectors.
type Registry struct {
	reg *prometheus.Registry
}

// New builds a registry over the given pool.
//
// The Go runtime and process collectors are included: memory and goroutine counts
// are the first thing anyone asks for when a worker is behaving oddly, and they
// cost nothing.
func New(pool *pgxpool.Pool, log *slog.Logger) *Registry {
	reg := prometheus.NewRegistry()

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		OutboundResponses,
		OutboundFailures,
	)

	if pool != nil {
		reg.MustRegister(&archiveCollector{pool: pool, log: log})
	}

	return &Registry{reg: reg}
}

// Handler serves the metrics endpoint.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		// A failing database should make the scrape report the failure, not
		// return a 500 and lose the process metrics that still work.
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// Serve runs the metrics listener until ctx is canceled.
//
// A failure to bind is returned rather than swallowed, but the caller is expected
// to treat it as non-fatal: an archive that cannot publish metrics is still an
// archive, and refusing to serve articles because a monitoring port is taken would
// be the wrong trade.
func (r *Registry) Serve(ctx context.Context, addr string, log *slog.Logger) error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", r.Handler())

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("listening for metrics on %s: %w", addr, err)
	}

	log.Info("metrics listening", "addr", ln.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	<-errCh
	return nil
}
