package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// A positional argument is found wherever it was written.
//
// `tome domain-rule set example.com --selector .post` printed usage for two
// releases, and `tome user link jane --base-url https://…` silently dropped the
// flag: Go's parser stops at the first non-flag word, so everything after the name
// was a stray argument. Both were documented as "flags first" rather than fixed,
// which is the shape of thing 1.0 should not freeze into a CLI.
func TestParsePositionalTakesEitherOrder(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		wantValue    string
		wantSelector string
		wantStrip    string
	}{
		{
			name:         "flags first",
			args:         []string{"--selector", ".post", "example.com"},
			wantValue:    "example.com",
			wantSelector: ".post",
		},
		{
			name:         "positional first",
			args:         []string{"example.com", "--selector", ".post"},
			wantValue:    "example.com",
			wantSelector: ".post",
		},
		{
			name:         "flags on both sides",
			args:         []string{"--strip", ".promo", "example.com", "--selector", ".post"},
			wantValue:    "example.com",
			wantSelector: ".post",
			wantStrip:    ".promo",
		},
		{
			name:      "no flags at all",
			args:      []string{"example.com"},
			wantValue: "example.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("domain-rule set", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			selector := fs.String("selector", "", "")
			strip := fs.String("strip", "", "")

			got, ok := parsePositional(fs, tc.args, "domain", io.Discard)
			if !ok {
				t.Fatalf("parsePositional(%q) refused the arguments", tc.args)
			}
			if got != tc.wantValue {
				t.Errorf("domain = %q, want %q", got, tc.wantValue)
			}
			if *selector != tc.wantSelector {
				t.Errorf("--selector = %q, want %q: a flag after the positional was dropped", *selector, tc.wantSelector)
			}
			if *strip != tc.wantStrip {
				t.Errorf("--strip = %q, want %q", *strip, tc.wantStrip)
			}
		})
	}
}

// What is refused, and whether the complaint names the right thing.
func TestParsePositionalComplaints(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"nothing at all", []string{}, "a domain is required"},
		{"flags but no positional", []string{"--selector", ".post"}, "a domain is required"},
		{"two positionals", []string{"example.com", "example.org"}, `got "example.org" as well`},
		{"a positional on each side", []string{"example.com", "--selector", ".p", "example.org"}, `got "example.org" as well`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("domain-rule set", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			fs.String("selector", "", "")

			var complaint strings.Builder
			if _, ok := parsePositional(fs, tc.args, "domain", &complaint); ok {
				t.Fatalf("parsePositional(%q) accepted arguments it should refuse", tc.args)
			}
			// The noun is the caller's, so a domain command complains about a domain
			// rather than about the username this helper was written for.
			if !strings.Contains(complaint.String(), tc.want) {
				t.Errorf("complaint = %q, want it to mention %q", complaint.String(), tc.want)
			}
		})
	}
}
