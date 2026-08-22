package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/runlevel-six/tomekeeper/internal/auth"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// user manages accounts from the command line.
//
// The interface can do all of this too, and this exists anyway because the
// interface needs somebody to be signed in — and the case that most needs fixing
// is the one where nobody can be. A forgotten password on the only administrator's
// account is otherwise a hand-written UPDATE against the database.
func user(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		userUsage(stderr)
		return exitUsage
	}

	switch args[0] {
	case "list":
		return userList(args[1:], stdout, stderr)
	case "add":
		return userAdd(args[1:], stdout, stderr)
	case "passwd":
		return userPasswd(args[1:], stdout, stderr)
	case "link":
		return userLink(args[1:], stdout, stderr)
	case "rm", "remove", "delete":
		return userRemove(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "tome user: unknown action %q\n\n", args[0])
		userUsage(stderr)
		return exitUsage
	}
}

func userUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: tome user <action>

Actions:
  list                     List every account
  add <name> [--admin]     Create an account with no password
  link <name>              Issue a single-use link for setting a password
  passwd <name>            Set a password directly, read from standard input
  rm <name>                Delete an account, keeping the archive

Flags for add:
  --admin                  Make this account an administrator

Flags for passwd:
  --password <text>        Take the password from the flag rather than from
                           standard input. It is then visible in the shell
                           history and in the process list.

An account starts with no password and cannot be signed in to until one is set.

`+"`link`"+` is the better way to do that, and is what to reach for when nobody can
sign in at all: hand over the URL it prints and the reader sets their own password,
which nobody else ever sees. `+"`passwd`"+` reads from standard input and does not hide
what you type, so it is for scripts and for the case where handing over a URL is
not possible.

Setting a password signs that reader out of every browser session and disconnects
their mobile clients, because the API key is derived from it.
`)
}

func userList(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		return usageError(stderr, "user list", args[0])
	}

	return withStore(stderr, func(s *store.Store) int {
		ctx, stop := signalContext()
		defer stop()

		accounts, err := s.System().ListAccounts(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "tome user: %v\n", err)
			return exitFailure
		}

		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tROLE\tPASSWORD\tFEEDS\tLINK")
		for _, a := range accounts {
			password := "set"
			if !a.HasPassword {
				password = "none"
			}
			link := "-"
			if a.PendingLink != nil {
				link = "expires " + a.PendingLink.Local().Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", a.Username, a.Role, password, a.Feeds, link)
		}
		return flushTable(tw, stderr)
	})
}

func userAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	admin := fs.Bool("admin", false, "make this account an administrator")
	name, ok := parseWithName(fs, args, stderr)
	if !ok {
		return exitUsage
	}

	role := store.RoleReader
	if *admin {
		role = store.RoleAdmin
	}

	return withStore(stderr, func(s *store.Store) int {
		ctx, stop := signalContext()
		defer stop()

		id, err := s.System().CreateUser(ctx, name, role)
		if err != nil {
			fmt.Fprintf(stderr, "tome user add: %v\n", err)
			return exitFailure
		}

		fmt.Fprintf(stdout, "created %q as %s, id %d\n", name, role, id)
		fmt.Fprintf(stdout, "it has no password yet; `tome user link %s` issues one they can set themselves\n",
			name)
		return exitOK
	})
}

func userLink(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user link", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base-url", "", "the archive's public URL, for printing a complete link")
	name, ok := parseWithName(fs, args, stderr)
	if !ok {
		return exitUsage
	}

	return withStore(stderr, func(s *store.Store) int {
		ctx, stop := signalContext()
		defer stop()

		id, err := s.System().LookupUser(ctx, name)
		if err != nil {
			return noSuchUser(stderr, "user link", name, err)
		}

		link, err := s.System().IssueSetupLink(ctx, id)
		if err != nil {
			fmt.Fprintf(stderr, "tome user link: %v\n", err)
			return exitFailure
		}

		path := "/set-password?token=" + link.Token
		if *base != "" {
			path = *base + path
		}
		fmt.Fprintln(stdout, path)
		fmt.Fprintf(stderr, "usable once, until %s\n", link.ExpiresAt.Local().Format(time.RFC3339))
		fmt.Fprintln(stderr, "any earlier link for this account has stopped working")
		return exitOK
	})
}

func userPasswd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user passwd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	password := fs.String("password", "", "the new password, rather than being prompted")
	name, ok := parseWithName(fs, args, stderr)
	if !ok {
		return exitUsage
	}

	chosen := *password
	if chosen == "" {
		var err error
		chosen, err = readPassword(stderr, fmt.Sprintf("New password for %q: ", name))
		if err != nil {
			fmt.Fprintf(stderr, "tome user passwd: %v\n", err)
			return exitFailure
		}
	}
	if chosen == "" {
		fmt.Fprintln(stderr, "tome user passwd: an empty password would lock the account rather than open it")
		return exitFailure
	}

	return withStore(stderr, func(s *store.Store) int {
		ctx, stop := signalContext()
		defer stop()

		id, err := s.System().LookupUser(ctx, name)
		if err != nil {
			return noSuchUser(stderr, "user passwd", name, err)
		}

		hash, err := auth.Hash(chosen)
		if err != nil {
			fmt.Fprintf(stderr, "tome user passwd: %v\n", err)
			return exitFailure
		}
		if err := s.System().SetPassword(ctx, id, hash, auth.FeverAPIKey(name, chosen)); err != nil {
			fmt.Fprintf(stderr, "tome user passwd: %v\n", err)
			return exitFailure
		}

		fmt.Fprintf(stdout, "password set for %q\n", name)
		fmt.Fprintln(stdout, "their browser sessions were signed out and mobile clients need the new password")
		return exitOK
	})
}

func userRemove(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "tome user rm: exactly one username is required")
		return exitUsage
	}
	name := args[0]

	return withStore(stderr, func(s *store.Store) int {
		ctx, stop := signalContext()
		defer stop()

		id, err := s.System().LookupUser(ctx, name)
		if err != nil {
			return noSuchUser(stderr, "user rm", name, err)
		}

		if err := s.System().DeleteUser(ctx, id); err != nil {
			if errors.Is(err, store.ErrLastAdmin) {
				fmt.Fprintf(stderr,
					"tome user rm: %q is the only administrator, and an archive without one "+
						"cannot make another through the interface\n", name)
				return exitFailure
			}
			fmt.Fprintf(stderr, "tome user rm: %v\n", err)
			return exitFailure
		}

		fmt.Fprintf(stdout, "deleted %q, with their subscriptions, tags and reading state\n", name)
		fmt.Fprintln(stdout, "every article and image is kept; `tome prune` reports what nothing references now")
		return exitOK
	})
}

// parseWithName reads a subcommand's flags and its one positional argument, in
// either order.
//
// Go's flag package stops at the first non-flag argument, so `tome user link jane
// --base-url https://…` silently ignores the flag and then complains about the
// argument count. That trap already costs attempts on `tome domain-rule set`,
// where the answer was to document flags-first; documenting it again is what this
// avoids. Parse once to take whatever came before the name, then parse the rest.
func parseWithName(fs *flag.FlagSet, args []string, stderr io.Writer) (string, bool) {
	if err := fs.Parse(args); err != nil {
		return "", false
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintf(stderr, "tome %s: a username is required\n", fs.Name())
		return "", false
	}

	name := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		return "", false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "tome %s: expected one username, got %q as well\n", fs.Name(), fs.Arg(0))
		return "", false
	}
	return name, true
}

// readPassword takes one line from standard input.
//
// It does not hide what is typed, and says so rather than pretending. Turning off
// terminal echo means golang.org/x/term, and a dependency exists here to be
// justified — this one would buy a nicety on the path that `tome user link`
// already covers better, since that hands over a URL and nobody learns the
// password at all.
func readPassword(prompt io.Writer, label string) (string, error) {
	fmt.Fprint(prompt, label)

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("reading the password: %w", err)
		}
		return "", errors.New("no password was given")
	}
	fmt.Fprintln(prompt)
	return strings.TrimRight(scanner.Text(), "\r\n"), nil
}

// flushTable writes out a tabwriter and reports a failure rather than dropping it.
func flushTable(tw *tabwriter.Writer, stderr io.Writer) int {
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "tome user: writing the table failed: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// noSuchUser reports a missing account as a plain answer rather than as a
// database error, and anything else as itself.
func noSuchUser(stderr io.Writer, cmd, name string, err error) int {
	if errors.Is(err, pgx.ErrNoRows) {
		fmt.Fprintf(stderr, "tome %s: no account named %q\n", cmd, name)
		return exitFailure
	}
	fmt.Fprintf(stderr, "tome %s: %v\n", cmd, err)
	return exitFailure
}
