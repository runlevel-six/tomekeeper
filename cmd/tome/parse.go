package main

import (
	"flag"
	"fmt"
	"io"
)

// parsePositional reads a subcommand's flags and its one positional argument, in
// either order.
//
// Go's flag package stops at the first non-flag argument, so
// `tome user link jane --base-url https://…` silently ignored the flag and then
// complained about the argument count, and `tome domain-rule set example.com
// --selector .post` printed usage as though the command were malformed. Both were
// documented as "flags first" for a while, which is a way of asking every reader to
// remember something the program could simply accept.
//
// Parse once to take whatever came before the name, then parse the rest. The noun is
// the caller's, so the complaint names what was actually missing rather than
// whichever subcommand happened to be written first.
func parsePositional(fs *flag.FlagSet, args []string, noun string, stderr io.Writer) (string, bool) {
	if err := fs.Parse(args); err != nil {
		return "", false
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintf(stderr, "tome %s: a %s is required\n", fs.Name(), noun)
		return "", false
	}

	value := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		return "", false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "tome %s: expected one %s, got %q as well\n", fs.Name(), noun, fs.Arg(0))
		return "", false
	}
	return value, true
}
