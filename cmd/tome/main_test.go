package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the binary's operational contract: exit codes and the
// messages an operator sees. The M0 acceptance criterion — "exits nonzero with
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

// The M0 acceptance criterion. t.Setenv guarantees the variable is absent for
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
