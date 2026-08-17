package store_test

import (
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/auth"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// cheapHash keeps these tests off the 19 MiB default cost. What is under test is
// the storage, not the KDF.
func cheapHash(t *testing.T, password string) string {
	t.Helper()

	p := auth.DefaultParams()
	p.Memory = 8 * 1024
	p.Iterations = 1

	hash, err := auth.HashWith(p, password)
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	return hash
}

// A seeded user has no password until one is set, and that state has to be
// distinguishable from a wrong password so the operator is told to run
// `tome migrate` with TOME_PASSWORD rather than hunting for a typo.
func TestCredentialsBeforeAPasswordIsSet(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	id, hash, err := s.System().Credentials(t.Context(), "tome")
	if err != nil {
		t.Fatalf("Credentials() = %v", err)
	}
	if id != store.SeedUserID {
		t.Errorf("id = %d, want %d", id, store.SeedUserID)
	}
	if hash != "" {
		t.Errorf("hash = %q for a user with no password, want empty", hash)
	}

	// And an empty hash must never verify, whatever is offered against it.
	if ok, err := auth.Verify(hash, ""); ok || err == nil {
		t.Errorf("Verify(empty hash) = %v, %v; want false and an error", ok, err)
	}
}

func TestSetPasswordRoundTrip(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	hash := cheapHash(t, "hunter2")
	key := auth.FeverAPIKey("tome", "hunter2")

	if err := s.System().SetPassword(ctx, userID, hash, key); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}

	gotID, gotHash, err := s.System().Credentials(ctx, "tome")
	if err != nil {
		t.Fatalf("Credentials() = %v", err)
	}
	if gotID != userID {
		t.Errorf("id = %d, want %d", gotID, userID)
	}

	ok, err := auth.Verify(gotHash, "hunter2")
	if err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	if !ok {
		t.Error("the stored hash does not verify against the password it was made from")
	}
	if ok, _ := auth.Verify(gotHash, "hunter3"); ok {
		t.Error("the stored hash verified against the wrong password")
	}
}

// The invariant §5.8 turns on: the hash and the Fever key are derived from the
// same cleartext, so they must move together. If a rotation updated only the
// hash, Fever clients would keep authenticating with the old password and
// nothing would say so.
func TestSetPasswordRotatesTheFeverKeyToo(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	storedKey := func() string {
		t.Helper()
		var key string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(api_key, '') FROM users WHERE id = $1`, userID).Scan(&key); err != nil {
			t.Fatalf("reading api_key: %v", err)
		}
		return key
	}

	if err := s.System().SetPassword(ctx, userID,
		cheapHash(t, "first"), auth.FeverAPIKey("tome", "first")); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}
	firstKey := storedKey()
	if firstKey == "" {
		t.Fatal("api_key is empty after setting a password")
	}

	_, firstHash, err := s.System().Credentials(ctx, "tome")
	if err != nil {
		t.Fatalf("Credentials() = %v", err)
	}

	if err := s.System().SetPassword(ctx, userID,
		cheapHash(t, "second"), auth.FeverAPIKey("tome", "second")); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}
	secondKey := storedKey()

	_, secondHash, err := s.System().Credentials(ctx, "tome")
	if err != nil {
		t.Fatalf("Credentials() = %v", err)
	}

	if secondHash == firstHash {
		t.Error("the password hash did not change on rotation")
	}
	if secondKey == firstKey {
		t.Error("the Fever api_key did not change on rotation; clients would keep using the old password")
	}
	if ok, _ := auth.Verify(secondHash, "first"); ok {
		t.Error("the old password still verifies after rotation")
	}
	if ok, _ := auth.Verify(secondHash, "second"); !ok {
		t.Error("the new password does not verify after rotation")
	}
}

func TestSetPasswordRefusesIncompletePairs(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	hash := cheapHash(t, "pw")

	if err := s.System().SetPassword(ctx, userID, "", auth.FeverAPIKey("tome", "pw")); err == nil {
		t.Error("SetPassword() accepted an empty hash")
	}
	if err := s.System().SetPassword(ctx, userID, hash, ""); err == nil {
		t.Error("SetPassword() accepted an empty Fever key, which would leave the pair inconsistent")
	}
	if err := s.System().SetPassword(ctx, 9999, hash, "abc"); err == nil {
		t.Error("SetPassword() silently did nothing for a user that does not exist")
	}
}
