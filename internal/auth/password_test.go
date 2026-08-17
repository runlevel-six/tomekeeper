package auth_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/auth"
)

// testParams keeps the suite fast. Every property under test is independent of
// the cost, and 19 MiB per call across a table of cases is seconds of pointless
// memory churn.
func testParams() auth.Params {
	p := auth.DefaultParams()
	p.Memory = 8 * 1024
	p.Iterations = 1
	return p
}

func TestHashVerifyRoundTrip(t *testing.T) {
	const password = "correct horse battery staple"

	encoded, err := auth.HashWith(testParams(), password)
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}

	ok, err := auth.Verify(encoded, password)
	if err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	if !ok {
		t.Error("Verify() = false for the correct password")
	}

	ok, err = auth.Verify(encoded, password+" ")
	if err != nil {
		t.Fatalf("Verify() with a wrong password = %v, want no error", err)
	}
	if ok {
		t.Error("Verify() accepted a password differing by one trailing space")
	}
}

// The salt has to be per-hash, or two users with the same password would be
// visibly identical in the table and one cracked hash would be two.
func TestHashIsSaltedPerCall(t *testing.T) {
	p := testParams()

	first, err := auth.HashWith(p, "same password")
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	second, err := auth.HashWith(p, "same password")
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}

	if first == second {
		t.Errorf("two hashes of the same password are identical, so the salt is not random:\n%s", first)
	}
	for _, encoded := range []string{first, second} {
		ok, err := auth.Verify(encoded, "same password")
		if err != nil || !ok {
			t.Errorf("Verify(%q) = %v, %v; both hashes must verify", encoded, ok, err)
		}
	}
}

// The encoding is the PHC format other argon2 implementations read, and it has
// to carry the parameters so that raising the cost later is not a lockout.
func TestEncodingCarriesItsParameters(t *testing.T) {
	p := testParams()

	encoded, err := auth.HashWith(p, "pw")
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}

	if !strings.HasPrefix(encoded, "$argon2id$v=19$") {
		t.Errorf("encoded hash does not start with the PHC argon2id prefix: %s", encoded)
	}
	if !strings.Contains(encoded, "m=8192,t=1,p=1") {
		t.Errorf("encoded hash does not record the cost it was made with: %s", encoded)
	}
	if n := strings.Count(encoded, "$"); n != 5 {
		t.Errorf("encoded hash has %d separators, want 5: %s", n, encoded)
	}
}

// A hash made at a lower cost must keep verifying after the default rises.
// Otherwise a cost increase locks the maintainer out of their own archive.
func TestVerifyUsesTheHashesOwnCost(t *testing.T) {
	weak := auth.Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

	encoded, err := auth.HashWith(weak, "pw")
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}

	// Verify consults the hash, not DefaultParams, which is stronger than weak.
	ok, err := auth.Verify(encoded, "pw")
	if err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	if !ok {
		t.Error("a hash made at a lower cost stopped verifying, which is a lockout")
	}

	needs, err := auth.NeedsRehash(encoded)
	if err != nil {
		t.Fatalf("NeedsRehash() = %v", err)
	}
	if !needs {
		t.Error("NeedsRehash() = false for a hash weaker than the default")
	}

	current, err := auth.Hash("pw")
	if err != nil {
		t.Fatalf("Hash() = %v", err)
	}
	if needs, err = auth.NeedsRehash(current); err != nil || needs {
		t.Errorf("NeedsRehash(current default) = %v, %v; want false, nil", needs, err)
	}
}

func TestEmptyPasswordIsRefused(t *testing.T) {
	if _, err := auth.HashWith(testParams(), ""); err == nil {
		t.Error("HashWith() accepted an empty password")
	}
}

// A corrupt row is an operational fault, not a failed login, and the two must
// not be confused: returning "wrong password" for a mangled hash would send
// someone hunting for a typo instead of a broken row.
func TestMalformedHashesAreDistinguishable(t *testing.T) {
	valid, err := auth.HashWith(testParams(), "pw")
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	parts := strings.Split(valid, "$")

	cases := map[string]string{
		"empty":              "",
		"not PHC at all":     "hunter2",
		"too few fields":     "$argon2id$v=19$m=8192,t=1,p=1$" + parts[4],
		"wrong algorithm":    "$argon2i$v=19$m=8192,t=1,p=1$" + parts[4] + "$" + parts[5],
		"unknown version":    "$argon2id$v=13$m=8192,t=1,p=1$" + parts[4] + "$" + parts[5],
		"unreadable cost":    "$argon2id$v=19$m=lots,t=1,p=1$" + parts[4] + "$" + parts[5],
		"undecodable salt":   "$argon2id$v=19$m=8192,t=1,p=1$!!!!$" + parts[5],
		"bcrypt by accident": "$2y$10$abcdefghijklmnopqrstuv",
	}

	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := auth.Verify(encoded, "pw")
			if ok {
				t.Errorf("Verify() accepted a malformed hash")
			}
			if !errors.Is(err, auth.ErrMalformedHash) {
				t.Errorf("Verify() = %v, want it to wrap ErrMalformedHash so a corrupt row is not reported as a wrong password", err)
			}
		})
	}
}

// The Fever key is a protocol constant, not an implementation detail: a client
// computes it independently and the two have to agree exactly.
func TestFeverAPIKeyMatchesTheProtocol(t *testing.T) {
	// Independently computed, so this asserts the protocol rather than whatever
	// the implementation happens to produce:
	//
	//	printf 'tome:hunter2' | md5sum
	const want = "12a8fc53dbf7728bd971c398941ad4af"

	got := auth.FeverAPIKey("tome", "hunter2")
	if len(got) != 32 {
		t.Errorf("FeverAPIKey() = %q, want 32 hex characters", got)
	}
	if got != want {
		t.Errorf("FeverAPIKey(\"tome\", \"hunter2\") = %s, want %s — every Fever client computes this independently", got, want)
	}
}

// Changing either input must change the key, or a password rotation would leave
// old clients working.
func TestFeverAPIKeyChangesWithItsInputs(t *testing.T) {
	base := auth.FeverAPIKey("tome", "hunter2")
	for name, got := range map[string]string{
		"different password": auth.FeverAPIKey("tome", "hunter3"),
		"different username": auth.FeverAPIKey("emot", "hunter2"),
	} {
		if got == base {
			t.Errorf("%s produced the same Fever key", name)
		}
	}
}
