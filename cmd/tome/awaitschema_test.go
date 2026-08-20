package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/db"
)

// The decision one attempt produces, which is the whole of this command that is
// worth testing without a database.
//
// A table rather than a live schema, deliberately: exercising the "behind" case
// against Postgres would mean driving the migration table backwards, and every
// package's integration tests share one database — a test that rewinds
// goose_db_version breaks whatever else is running. That mistake has already been
// made once in this project.
func TestEvaluateSchema(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    db.SchemaState
		err      error
		wantDone bool
		mentions []string
	}{
		{
			name:     "current",
			state:    db.SchemaState{Applied: 6, Expected: 6},
			wantDone: true,
		},
		{
			// A rollback in progress: the old binary's queries still work against a
			// superset schema, so this must not hold a pod in Init forever.
			name:     "newer than this build needs",
			state:    db.SchemaState{Applied: 7, Expected: 6},
			wantDone: true,
		},
		{
			name:     "behind",
			state:    db.SchemaState{Applied: 5, Expected: 6},
			wantDone: false,
			mentions: []string{"5", "6", "migration"},
		},
		{
			// The first deploy, where nothing has ever been migrated.
			name:     "nothing applied at all",
			state:    db.SchemaState{Applied: 0, Expected: 6},
			wantDone: false,
			mentions: []string{"0", "6"},
		},
		{
			// Waited on rather than failed: Postgres still starting is the most
			// common thing this sees, and it fixes itself.
			name:     "the database is not answering",
			err:      errors.New("connecting: dial tcp 10.0.0.1:5432: connect: connection refused"),
			wantDone: false,
			mentions: []string{"not answering", "connection refused"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateSchema(tc.state, tc.err)

			if got.done != tc.wantDone {
				t.Errorf("done = %v, want %v (reason: %s)", got.done, tc.wantDone, got.reason)
			}
			if got.done && got.reason != "" {
				t.Errorf("a finished decision carries a reason: %q", got.reason)
			}
			if !got.done && got.reason == "" {
				t.Error("an unfinished decision carries no reason, so the log line would be empty")
			}
			for _, want := range tc.mentions {
				if !strings.Contains(got.reason, want) {
					t.Errorf("reason %q does not mention %q, so an operator cannot act on it",
						got.reason, want)
				}
			}
		})
	}
}

func TestAwaitSchemaRejectsBadInvocations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		mention string
	}{
		{"a positional argument", []string{"now"}, "Usage:"},
		{"an unknown flag", []string{"--forever"}, "flag provided but not defined"},
		{"a zero interval", []string{"--interval", "0s"}, "must be positive"},
		{"a negative interval", []string{"--interval", "-2s"}, "must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No database URL is needed: every one of these must be refused before
			// configuration is read, or an operator debugging a typo gets a
			// complaint about the database instead.
			t.Setenv("TOME_DATABASE_URL", "")

			var stdout, stderr bytes.Buffer
			if code := awaitSchema(tc.args, &stdout, &stderr); code != exitUsage {
				t.Errorf("awaitSchema(%q) = %d, want %d", tc.args, code, exitUsage)
			}
			if !strings.Contains(stderr.String(), tc.mention) {
				t.Errorf("stderr = %q, want it to mention %q", stderr.String(), tc.mention)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty on a usage failure", stdout.String())
			}
		})
	}
}

// The subcommand has to be reachable, and named in the help — an initContainer
// referencing a subcommand the binary does not have fails with a usage error
// nobody reads until the pod will not start.
func TestAwaitSchemaIsDispatchedAndDocumented(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("run([help]) = %d", code)
	}
	if !strings.Contains(stdout.String(), "await-schema") {
		t.Error("await-schema is not listed in the usage text")
	}

	// Dispatched rather than falling through to "unknown subcommand".
	t.Setenv("TOME_DATABASE_URL", "")
	var out, errOut bytes.Buffer
	code := run([]string{"await-schema"}, &out, &errOut)
	if strings.Contains(errOut.String(), "unknown subcommand") {
		t.Error("await-schema is not wired into the dispatch")
	}
	// With no database configured it is a configuration error, not a wait.
	if code != exitUsage {
		t.Errorf("run([await-schema]) with no database = %d, want %d", code, exitUsage)
	}
}
