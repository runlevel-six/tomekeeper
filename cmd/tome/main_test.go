package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the binary's operational contract: exit codes and the
// messages an operator sees. The acceptance criterion — "exits nonzero with
// a clear message when TOME_DATABASE_URL is unset" — is the last case here,
// and again end to end against a container in scripts/smoke.sh.
func TestRun(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string // substring of stdout
		wantErr  string // substring of stderr
	}{
		{
			name:     "no arguments prints usage and fails",
			args:     nil,
			wantCode: exitUsage,
			wantErr:  "Usage:",
		},
		{
			name:     "unknown subcommand names itself",
			args:     []string{"reticulate"},
			wantCode: exitUsage,
			wantErr:  `unknown subcommand "reticulate"`,
		},
		{
			name:     "version succeeds",
			args:     []string{"version"},
			wantCode: exitOK,
			wantOut:  "tomekeeper",
		},
		{
			name:     "help succeeds and goes to stdout",
			args:     []string{"help"},
			wantCode: exitOK,
			wantOut:  "Subcommands:",
		},
		{
			name:     "--help succeeds",
			args:     []string{"--help"},
			wantCode: exitOK,
			wantOut:  "Subcommands:",
		},
		{
			name:     "serve rejects arguments",
			args:     []string{"serve", "--port=9000"},
			wantCode: exitUsage,
			wantErr:  "unexpected argument",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if got := run(tt.args, &stdout, &stderr); got != tt.wantCode {
				t.Errorf("run(%q) = %d, want %d\nstderr: %s", tt.args, got, tt.wantCode, stderr.String())
			}
			if tt.wantOut != "" && !strings.Contains(stdout.String(), tt.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

// The acceptance criterion. t.Setenv guarantees the variable is absent for
// this test regardless of the developer's shell.
func TestServeFailsWithoutDatabaseURL(t *testing.T) {
	t.Setenv("TOME_DATABASE_URL", "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"serve"}, &stdout, &stderr)

	if code == exitOK {
		t.Fatal("run([serve]) = 0 with no TOME_DATABASE_URL, want nonzero")
	}
	if code != exitUsage {
		t.Errorf("run([serve]) = %d, want %d (configuration error)", code, exitUsage)
	}

	msg := stderr.String()
	for _, want := range []string{"TOME_DATABASE_URL", "is required", "postgres://"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr does not contain %q, so the message is not actionable:\n%s", want, msg)
		}
	}
	// Nothing should reach stdout: this is a failure, and stdout may be piped.
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty on a configuration failure", stdout.String())
	}
}

// A configuration error must be reported even when other settings are also
// wrong — the operator should not have to fix them one at a time.
func TestServeReportsEveryConfigProblem(t *testing.T) {
	t.Setenv("TOME_DATABASE_URL", "")
	t.Setenv("TOME_LOG_FORMAT", "logfmt")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"serve"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("run([serve]) = %d, want %d", code, exitUsage)
	}

	msg := stderr.String()
	for _, want := range []string{"TOME_DATABASE_URL", "TOME_LOG_FORMAT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr does not mention %s:\n%s", want, msg)
		}
	}
}

// `import-opml --dry-run` deliberately needs no database, so someone deciding
// whether to trust this with a long-curated subscription list can see exactly
// what it would do before configuring anything.
func TestImportOPMLDryRunNeedsNoDatabase(t *testing.T) {
	t.Setenv("TOME_DATABASE_URL", "")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"import-opml", "--dry-run",
		filepath.Join("..", "..", "internal", "feed", "testdata", "opml", "freshrss.opml"),
	}, &stdout, &stderr)

	if code != exitOK {
		t.Fatalf("run() = %d, want %d\nstderr: %s", code, exitOK, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"dry run, nothing written", "7 subscriptions", "Example Engineering", "News/Local"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

func TestImportOPMLRejectsBadInvocations(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no file", []string{"import-opml"}},
		{"two files", []string{"import-opml", "a.opml", "b.opml"}},
		{"missing file", []string{"import-opml", "--dry-run", "does-not-exist.opml"}},
		{"not OPML", []string{"import-opml", "--dry-run", "main.go"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != exitUsage {
				t.Errorf("run(%q) = %d, want %d\nstderr: %s", tt.args, code, exitUsage, stderr.String())
			}
		})
	}
}

// The flag was called --since-version, which reads as an ordering — "everything
// from version 2 onwards" — and is not one: the selection is "any version other
// than this". Passing the version everything already has therefore selects
// nothing and reports that everything is up to date, which is true and exactly
// the wrong thing to hear.
//
// This happened for real. The fix is a name that says what it means, an alias so
// written-down commands keep working, and a message that names the likely
// mistake.
func TestReextractFlagNaming(t *testing.T) {
	var out, errOut bytes.Buffer

	// Usage must advertise the honest name.
	reextract([]string{"--help"}, &out, &errOut)
	usage := out.String() + errOut.String()

	if !strings.Contains(usage, "--target-version") {
		t.Errorf("usage does not mention --target-version:\n%s", usage)
	}
	if !strings.Contains(usage, "deprecated alias") {
		t.Errorf("usage does not mark --since-version as deprecated, so nothing tells a reader to stop using it:\n%s", usage)
	}
}

// Both spellings must reach the same setting; the alias exists so that commands
// already written down keep working.
func TestReextractAcceptsBothFlagSpellings(t *testing.T) {
	for _, flag := range []string{"--target-version=9", "--since-version=9"} {
		var out, errOut bytes.Buffer
		reextract([]string{flag}, &out, &errOut)

		// Asserted on the message rather than the exit code: with no database
		// configured this fails at configuration, which also exits with a usage
		// code, so the code alone cannot tell an unknown flag from a missing
		// setting. "flag provided but not defined" is what an unknown flag says
		// and is the only thing being ruled out here.
		if strings.Contains(errOut.String(), "not defined") {
			t.Errorf("%s is not a recognized flag:\n%s", flag, errOut.String())
		}
	}
}

// A command that spends requests on somebody else's server must refuse a bad
// invocation before it reaches the database, let alone the network.
//
// Every case here returns without opening a connection, which is what makes them
// runnable with no configuration at all — and is the property worth keeping: a
// mistyped id should cost a message, not eight requests to a site that did not ask
// for them.
func TestRefetchRejectsBadInvocations(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no article ids", nil, "Usage:"},
		{"a word instead of an id", []string{"comics"}, "not an article id"},
		{"a negative id", []string{"-3"}, "flag provided but not defined"},
		{"zero", []string{"0"}, "not an article id"},
		{"one good id and one bad", []string{"12", "twelve"}, "not an article id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut strings.Builder
			if code := refetch(tc.args, &out, &errOut); code != exitUsage {
				t.Errorf("refetch(%v) = %d, want %d — a bad invocation must not proceed",
					tc.args, code, exitUsage)
			}
			if got := errOut.String(); !strings.Contains(got, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", got, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("a refused invocation wrote to stdout: %q", out.String())
			}
		})
	}
}
