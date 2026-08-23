package main

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/auth"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
)

// `tome migrate` runs on every deploy with TOME_PASSWORD present, and setting a
// password now revokes every session. So the check that the password actually
// changed is what stands between a routine deployment and signing the reader out
// of the web interface each time — which is why it is tested here rather than left
// to the command that is awkward to invoke.
func TestPasswordUnchanged(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	system := s.System()
	log := slog.New(slog.DiscardHandler)

	// Cheap parameters: this exercises the decision, not the KDF.
	p := auth.DefaultParams()
	p.Memory, p.Iterations = 8*1024, 1
	hash, err := auth.HashWith(p, "the stored password")
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}

	// Before a password exists there is nothing to compare, and the caller must go
	// on to write one.
	if unchanged, err := passwordUnchanged(t.Context(), system, "tome", "anything", log); err != nil {
		t.Fatalf("passwordUnchanged() before any password = %v", err)
	} else if unchanged {
		t.Error("reported unchanged for an account with no password, so a first run would set none")
	}

	if err := system.SetPassword(t.Context(), userID, hash,
		auth.FeverAPIKey("tome", "the stored password")); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}

	// The deploy case: the same password, which must not be rewritten. Note this
	// cannot be done by comparing hashes — argon2id salts randomly, so hashing the
	// same password again gives a different string every time.
	if unchanged, err := passwordUnchanged(t.Context(), system, "tome", "the stored password", log); err != nil {
		t.Fatalf("passwordUnchanged() = %v", err)
	} else if !unchanged {
		t.Error("reported changed for the password already stored; every deploy would revoke sessions")
	}

	// A genuinely different password has to be written.
	if unchanged, err := passwordUnchanged(t.Context(), system, "tome", "a different password", log); err != nil {
		t.Fatalf("passwordUnchanged() = %v", err)
	} else if unchanged {
		t.Error("reported unchanged for a different password, so a rotation would be ignored")
	}

	// An unknown account is not an error here: the seed step creates it, and this
	// runs afterwards.
	if unchanged, err := passwordUnchanged(t.Context(), system, "nobody", "anything", log); err != nil {
		t.Fatalf("passwordUnchanged() for an unknown user = %v", err)
	} else if unchanged {
		t.Error("reported unchanged for an account that does not exist")
	}

	// A hash that cannot be parsed is a broken row. Overwriting it is the repair,
	// so this must report changed rather than failing the migration.
	if _, err := s.Pool().Exec(t.Context(),
		`UPDATE users SET password_hash = $2 WHERE id = $1`, userID, "not a PHC string"); err != nil {
		t.Fatalf("corrupting the hash: %v", err)
	}
	if unchanged, err := passwordUnchanged(t.Context(), system, "tome", "anything", log); err != nil {
		t.Fatalf("passwordUnchanged() over a broken hash = %v, want it treated as changed", err)
	} else if unchanged {
		t.Error("reported unchanged over an unreadable hash, which would leave it unrepaired")
	}
}

// `tome migrate` must change nothing on an account whose reader has renamed it.
//
// This is the caller the test above could not protect. `passwordUnchanged` correctly
// reports "changed" for an account it cannot find — the seed step creates the account,
// so a missing one means there is no password yet — and migrate was passing it
// TOME_USERNAME rather than the name the account has. After a rename the lookup found
// nothing, so a deploy that changed nothing decided the password had changed: it
// revoked every browser session and rewrote the Fever API key, on every deploy, which
// is precisely what verifying first exists to prevent. Then it derived that key from
// the configured name, producing a credential for a username that no longer existed,
// so every mobile client was refused with nothing to explain it.
//
// Reported from production one release after the rename control shipped. The command is
// awkward to invoke, which is why neither bug had a test: this invokes it.
func TestMigrateLeavesARenamedAccountAlone(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	system := s.System()
	ctx := t.Context()

	const password = "the configured password"

	// Cheap parameters, because this exercises the decision rather than the KDF —
	// and because the path under test verifies rather than hashes when it is right.
	p := auth.DefaultParams()
	p.Memory, p.Iterations = 8*1024, 1
	hash, err := auth.HashWith(p, password)
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	if err := system.SetPassword(ctx, userID, hash, auth.FeverAPIKey("tome", password)); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}

	// The reader renames themselves from Settings, which rewrites the Fever key.
	if err := system.SetUsername(ctx, userID, "jason", auth.FeverAPIKey("jason", password)); err != nil {
		t.Fatalf("SetUsername() = %v", err)
	}

	read := func() (name, key string, epoch int64) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT username, COALESCE(api_key, ''), session_epoch FROM users WHERE id = $1`,
			userID).Scan(&name, &key, &epoch); err != nil {
			t.Fatalf("reading the account: %v", err)
		}
		return name, key, epoch
	}
	wantName, wantKey, wantEpoch := read()

	t.Setenv("TOME_DATABASE_URL", os.Getenv(dbtest.EnvVar))
	t.Setenv("TOME_USERNAME", "tome")
	t.Setenv("TOME_PASSWORD", password)

	var out, errOut bytes.Buffer
	if code := migrate(nil, &out, &errOut); code != exitOK {
		t.Fatalf("migrate() = %d\nstdout: %s\nstderr: %s", code, out.String(), errOut.String())
	}

	name, key, epoch := read()
	if name != wantName {
		t.Errorf("the account is called %q after a deploy, want %q", name, wantName)
	}
	if key != wantKey {
		t.Errorf("the Fever key was rewritten by a deploy that changed nothing:\n got %s\nwant %s\n"+
			"every mobile client would need reconnecting", key, wantKey)
	}
	if epoch != wantEpoch {
		t.Errorf("session epoch moved %d → %d, so the reader was signed out by a deploy",
			wantEpoch, epoch)
	}
	if !strings.Contains(out.String(), "already the configured one") {
		t.Errorf("migrate did not report the password as unchanged:\n%s", out.String())
	}
	// And it names the account rather than reading the setting back out.
	if !strings.Contains(out.String(), `"jason"`) || strings.Contains(out.String(), `password set for "tome"`) {
		t.Errorf("migrate talks about the configured name rather than the account's:\n%s", out.String())
	}
}

// When the password really has changed, the key it writes belongs to the account's own
// name — the second half of the same bug.
func TestMigrateDerivesTheFeverKeyFromTheAccountsName(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	system := s.System()
	ctx := t.Context()

	p := auth.DefaultParams()
	p.Memory, p.Iterations = 8*1024, 1
	hash, err := auth.HashWith(p, "the old password")
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	if err := system.SetPassword(ctx, userID, hash, auth.FeverAPIKey("tome", "the old password")); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}
	if err := system.SetUsername(ctx, userID, "jason", auth.FeverAPIKey("jason", "the old password")); err != nil {
		t.Fatalf("SetUsername() = %v", err)
	}

	const rotated = "the rotated password"
	t.Setenv("TOME_DATABASE_URL", os.Getenv(dbtest.EnvVar))
	t.Setenv("TOME_USERNAME", "tome")
	t.Setenv("TOME_PASSWORD", rotated)

	var out, errOut bytes.Buffer
	if code := migrate(nil, &out, &errOut); code != exitOK {
		t.Fatalf("migrate() = %d\nstdout: %s\nstderr: %s", code, out.String(), errOut.String())
	}

	var key string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(api_key, '') FROM users WHERE id = $1`, userID).Scan(&key); err != nil {
		t.Fatalf("reading the key: %v", err)
	}
	if want := auth.FeverAPIKey("jason", rotated); key != want {
		t.Errorf("the stored key is %s; want %s, computed from the name the account has.\n"+
			"Derived from TOME_USERNAME it is a credential for a username that does not exist, "+
			"and no client can be told why it was refused", key, want)
	}
}
