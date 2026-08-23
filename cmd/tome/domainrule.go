package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/jackc/pgx/v5"

	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// domainRule manages per-domain extraction overrides.
//
// The extraction tail is permanent: readability-class tools handle most sites,
// and the rest need hand-written rules forever. This command is what makes
// that routine maintenance — look at the failed-fetch queue, find the
// selector, write one rule — rather than a crisis.
func domainRule(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		domainRuleUsage(stderr)
		return exitUsage
	}

	switch args[0] {
	case "list":
		return domainRuleList(args[1:], stdout, stderr)
	case "set":
		return domainRuleSet(args[1:], stdout, stderr)
	case "rm", "remove", "delete":
		return domainRuleRemove(args[1:], stdout, stderr)
	case "show":
		return domainRuleShow(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "tome domain-rule: unknown action %q\n\n", args[0])
		domainRuleUsage(stderr)
		return exitUsage
	}
}

func domainRuleUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: tome domain-rule <action>

Actions:
  list                    List every rule
  show <domain>           Show the rule that would apply to a domain
  set <domain> [flags]    Create or replace a rule
  rm <domain>             Remove a rule

Flags for set:
  --selector <css>        CSS selector for the article body
  --strip <css>           Selector to remove before extraction (repeatable)
  --requires-js           Mark the domain as needing a headless render
  --rate <rps>            Per-host request rate, overriding TOME_FETCH_RPS
  --notes <text>          Why this rule exists

Rules apply to subdomains: a rule for example.com covers blog.example.com
unless that subdomain has a rule of its own.
`)
}

func domainRuleList(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		return usageError(stderr, "domain-rule list", args[0])
	}

	return withStore(stderr, func(s *store.Store) int {
		ctx, stop := signalContext()
		defer stop()

		rules, err := s.System().ListDomainRules(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "tome domain-rule: %v\n", err)
			return exitFailure
		}
		if len(rules) == 0 {
			fmt.Fprintln(stdout, "no domain rules")
			return exitOK
		}

		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "DOMAIN\tSELECTOR\tSTRIP\tJS\tRATE\tNOTES")
		for _, r := range rules {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				r.Domain,
				orDash(r.ContentSelector),
				orDash(strings.Join(r.StripSelectors, " ")),
				yesNo(r.RequiresJS),
				rateOrDash(r.RateLimitRPS),
				orDash(r.Notes))
		}
		_ = tw.Flush()
		return exitOK
	})
}

func domainRuleShow(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "Usage: tome domain-rule show <domain>")
		return exitUsage
	}
	host := args[0]

	return withStore(stderr, func(s *store.Store) int {
		ctx, stop := signalContext()
		defer stop()

		rule, err := s.System().DomainRuleFor(ctx, host)
		if errors.Is(err, pgx.ErrNoRows) {
			fmt.Fprintf(stdout, "no rule applies to %s\n", host)
			return exitOK
		}
		if err != nil {
			fmt.Fprintf(stderr, "tome domain-rule: %v\n", err)
			return exitFailure
		}

		// Naming the matched domain matters: a rule inherited from a parent
		// domain is easy to forget about when a subdomain misbehaves.
		fmt.Fprintf(stdout, "%s (matched by the rule for %s)\n", host, rule.Domain)
		fmt.Fprintf(stdout, "  selector: %s\n", orDash(rule.ContentSelector))
		fmt.Fprintf(stdout, "  strip:    %s\n", orDash(strings.Join(rule.StripSelectors, ", ")))
		fmt.Fprintf(stdout, "  requires js: %s\n", yesNo(rule.RequiresJS))
		fmt.Fprintf(stdout, "  rate:     %s\n", rateOrDash(rule.RateLimitRPS))
		fmt.Fprintf(stdout, "  notes:    %s\n", orDash(rule.Notes))
		return exitOK
	})
}

// stripList collects repeated --strip flags.
type stripList []string

func (s *stripList) String() string     { return strings.Join(*s, ", ") }
func (s *stripList) Set(v string) error { *s = append(*s, v); return nil }

func domainRuleSet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("domain-rule set", flag.ContinueOnError)
	fs.SetOutput(stderr)

	selector := fs.String("selector", "", "CSS selector for the article body")
	requiresJS := fs.Bool("requires-js", false, "the domain needs a headless render")
	rate := fs.Float64("rate", 0, "per-host request rate in requests per second")
	notes := fs.String("notes", "", "why this rule exists")

	var strip stripList
	fs.Var(&strip, "strip", "selector to remove before extraction (repeatable)")

	// Flags may come before or after the domain. Requiring them first was the
	// documented workaround for two releases, and a rule written the natural way
	// printed usage as though the command were wrong.
	domain, ok := parsePositional(fs, args, "domain", stderr)
	if !ok {
		fmt.Fprintln(stderr, "Usage: tome domain-rule set <domain> [flags]")
		fs.PrintDefaults()
		return exitUsage
	}

	return withStore(stderr, func(s *store.Store) int {
		ctx, stop := signalContext()
		defer stop()

		if err := s.System().UpsertDomainRule(ctx, store.DomainRule{
			Domain:          domain,
			ContentSelector: *selector,
			StripSelectors:  strip,
			RequiresJS:      *requiresJS,
			RateLimitRPS:    *rate,
			Notes:           *notes,
		}); err != nil {
			fmt.Fprintf(stderr, "tome domain-rule: %v\n", err)
			return exitFailure
		}

		fmt.Fprintf(stdout, "saved the rule for %s\n", strings.ToLower(domain))
		if *selector != "" {
			// The rule changes nothing until the affected articles are
			// reprocessed, and saying so here saves the "why is it still
			// wrong" round trip.
			// --target-version 0 rather than a bare reextract: reextract selects
			// on extractor version, so with every body already at the current
			// version a bare run finds nothing and the rule appears to do
			// nothing. Comparing against a version no body has selects all of
			// them.
			// Scoped to this domain, because it is the only site the rule can
			// affect, and reprocessing a large archive to correct one site is
			// hours of needless work.
			fmt.Fprintf(stdout,
				"run `tome reextract --target-version 0 --domain %s` to apply it to articles already stored\n",
				strings.ToLower(domain))
		}
		return exitOK
	})
}

func domainRuleRemove(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "Usage: tome domain-rule rm <domain>")
		return exitUsage
	}
	domain := args[0]

	return withStore(stderr, func(s *store.Store) int {
		ctx, stop := signalContext()
		defer stop()

		removed, err := s.System().DeleteDomainRule(ctx, domain)
		if err != nil {
			fmt.Fprintf(stderr, "tome domain-rule: %v\n", err)
			return exitFailure
		}
		if !removed {
			fmt.Fprintf(stderr, "no rule for %s\n", domain)
			return exitFailure
		}

		fmt.Fprintf(stdout, "removed the rule for %s\n", strings.ToLower(domain))
		return exitOK
	})
}

// withStore is the connect-run-close preamble the data commands share.
func withStore(stderr io.Writer, run func(*store.Store) int) int {
	cfg, log, code := loadConfigAndLogger(stderr)
	if code != exitOK {
		return code
	}

	ctx, stop := signalContext()
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("cannot reach the database", "error", err)
		return exitFailure
	}
	defer pool.Close()

	return run(store.New(pool))
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func rateOrDash(rps float64) string {
	if rps <= 0 {
		return "-"
	}
	return fmt.Sprintf("%g/s", rps)
}
