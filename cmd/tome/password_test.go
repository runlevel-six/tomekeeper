package main

import (
	"log/slog"
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
