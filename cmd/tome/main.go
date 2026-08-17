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

	case "worker":
		return worker(args[1:], stderr)

	case "migrate":
		return migrate(args[1:], stdout, stderr)

	case "import-opml":
		return importOPML(args[1:], stdout, stderr)

	case "reextract":
		return reextract(args[1:], stdout, stderr)

	case "domain-rule":
		return domainRule(args[1:], stdout, stderr)

	case "archive":
		return archiveCmd(args[1:], stdout, stderr)

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

func usage(w io.Writer) {
	fmt.Fprint(w, `tome — self-hosted feed aggregator and article archive

Usage:
  tome <subcommand>

Subcommands:
  serve         Run the HTTP server (web UI, health endpoints)
  worker        Run the background job pool (polling, fetching, extraction)
  migrate       Apply database migrations and seed the user
  import-opml   Add subscriptions from an OPML file
  reextract     Re-extract stored pages at the current extractor version
  domain-rule   Manage per-domain extraction overrides
  archive       Report on what the archive holds
  version       Print build version and exit
  help          Print this message

Configuration is read from `+config.Prefix+`-prefixed environment variables.
`+config.Prefix+`DATABASE_URL is required. Full reference: docs/reference/configuration.md
`)
}

// signalContext returns a context canceled by SIGINT or SIGTERM. SIGTERM is
// what Kubernetes sends; SIGINT is what a terminal sends.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
