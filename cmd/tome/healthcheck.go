package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// healthcheck asks a running server whether it is alive, and exits 0 or 1.
//
// This exists because the image is distroless: there is no shell, no curl and no
// wget inside it, so a container healthcheck has nothing to run. Kubernetes does not
// care — it makes HTTP probes itself — but Docker and Compose can only exec a
// command, which leaves two options: no healthcheck at all, or one that runs the
// binary and proves nothing.
//
// The second is worse than the first. `docker compose ps` reporting "healthy" for a
// server that is failing every request is a status line that lies, and a lying status
// line costs more than a missing one.
//
// Liveness rather than readiness, deliberately, matching what an orchestrator should
// restart on: /healthz answers without consulting the database, so a Postgres restart
// does not get every container killed and restarted alongside it. Use /readyz — or
// just the page — to ask whether it can serve.
func healthcheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "address to check (default: TOME_HTTP_ADDR, or :8080)")
	timeout := fs.Duration("timeout", 3*time.Second, "how long to wait for an answer")

	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: tome healthcheck [--addr host:port] [--timeout 3s]")
		fmt.Fprintln(stderr, "\nExits 0 when the server answers /healthz, 1 otherwise.")
		fmt.Fprintln(stderr, "\nFlags:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return exitUsage
	}

	// The raw environment variable rather than the full configuration, because a
	// healthcheck must not need a database URL to answer a question about a listening
	// socket — and because configuration that fails to validate is exactly when
	// somebody wants to know what the server is doing.
	target := *addr
	if target == "" {
		target = os.Getenv("TOME_HTTP_ADDR")
	}
	if target == "" {
		target = ":8080"
	}
	// A bare ":8080" is what a server binds; it is not something a client can dial.
	if strings.HasPrefix(target, ":") {
		target = "127.0.0.1" + target
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	url := "http://" + target + "/healthz"

	// The address is one the operator passed to a command the operator ran, which is
	// not the shape gosec's SSRF rule is about: there is no request handler here and
	// no caller but a shell or a container runtime. It reaches exactly where it was
	// pointed.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // operator-supplied address
	if err != nil {
		fmt.Fprintf(stderr, "tome healthcheck: %v\n", err)
		return exitFailure
	}

	resp, err := http.DefaultClient.Do(req) //nolint:gosec // operator-supplied address
	if err != nil {
		fmt.Fprintf(stderr, "tome healthcheck: %s: %v\n", url, err)
		return exitFailure
	}
	defer func() { _ = resp.Body.Close() }()

	// Drained so the connection can be reused, which matters not at all for one
	// request and costs one line.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "tome healthcheck: %s answered HTTP %d\n", url, resp.StatusCode)
		return exitFailure
	}

	fmt.Fprintln(stdout, "ok")
	return exitOK
}
