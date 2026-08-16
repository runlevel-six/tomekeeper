// Command tome is the tomekeeper binary: feed aggregator and article archive.
//
// One binary, several subcommands. `tome serve` runs the HTTP surface;
// `tome worker` (M1) runs the job pool. They are separate Deployments built
// from the same image.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/runlevel-six/tomekeeper/internal/config"
	"github.com/runlevel-six/tomekeeper/internal/logging"
	"github.com/runlevel-six/tomekeeper/internal/server"
	"github.com/runlevel-six/tomekeeper/internal/version"
)

// Exit codes. These are part of the operational contract: a supervisor should
// be able to tell "you configured me wrongly" from "something broke" without
// parsing log output.
const (
	exitOK      = 0
	exitFailure = 1 // runtime failure
	exitUsage   = 2 // bad invocation or invalid configuration
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's testable body: no globals, explicit streams, an int out.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}

	switch cmd := args[0]; cmd {
	case "serve":
		return serve(args[1:], stderr)

	case "version":
		fmt.Fprintln(stdout, version.String())
		return exitOK

	case "help", "-h", "--help":
		usage(stdout)
		return exitOK

	default:
		fmt.Fprintf(stderr, "tome: unknown subcommand %q\n\n", cmd)
		usage(stderr)
		return exitUsage
	}
}

// serve loads configuration, builds the logger and server, and blocks until a
// termination signal arrives.
func serve(args []string, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "tome serve: unexpected argument %q\n", args[0])
		fmt.Fprintf(stderr, "tome serve takes no flags; it is configured entirely by %s* environment variables.\n", config.Prefix)
		fmt.Fprintln(stderr, "See docs/reference/configuration.md.")
		return exitUsage
	}

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		// Written plainly to stderr, not through the structured logger: the
		// logger's own configuration is part of what just failed to validate,
		// and a human is reading this in a terminal or a crash-loop log.
		fmt.Fprintf(stderr, "tome: %v\n\n", err)
		fmt.Fprintln(stderr, "See docs/reference/configuration.md for every setting.")
		return exitUsage
	}

	log := logging.New(stderr, cfg.LogFormat, cfg.LogLevel)
	log.Info("starting", "version", version.Short(), "config", cfg)

	// SIGTERM is what Kubernetes sends; SIGINT is what a terminal sends.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No readiness checks are registered at M0 — there is no database
	// connection until M1. /readyz therefore reports ready as soon as the
	// process is serving, which is accurate for what this binary does today.
	srv := server.New(cfg, log)

	if err := srv.Run(ctx); err != nil {
		log.Error("server failed", "error", err)
		return exitFailure
	}
	return exitOK
}

func usage(w io.Writer) {
	fmt.Fprint(w, `tome — self-hosted feed aggregator and article archive

Usage:
  tome <subcommand>

Subcommands:
  serve      Run the HTTP server (web UI, health endpoints)
  version    Print build version and exit
  help       Print this message

Configuration is read from `+config.Prefix+`-prefixed environment variables.
`+config.Prefix+`DATABASE_URL is required. Full reference: docs/reference/configuration.md
`)
}
