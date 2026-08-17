// Package auth hashes and verifies the single user's password, and derives the
// Fever API key that has to be written alongside it.
//
// The two live together because they are produced from the same cleartext at the
// same moment and must never disagree. The reason is the Fever
// protocol's credential is MD5 of "username:password", and that cannot be
// recovered from an argon2 hash afterwards. So a password change that forgets to
// rewrite the key leaves every Fever client authenticating against a password
// that no longer exists, with nothing to log.
package auth

import (
	//nolint:gosec // MD5 is the Fever wire format, not password protection. See FeverAPIKey.
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ErrMalformedHash means a stored hash could not be parsed. It is not a wrong
// password: it is a corrupt or hand-edited row, and it should be treated as an
// operational fault rather than a failed login.
var ErrMalformedHash = errors.New("malformed password hash")

// Params are the argon2id cost parameters.
type Params struct {
	// Memory is the memory cost in KiB.
	Memory uint32
	// Iterations is the time cost.
	Iterations uint32
	// Parallelism is the number of lanes.
	Parallelism uint8
	// SaltLength and KeyLength are in bytes.
	SaltLength uint32
	KeyLength  uint32
}

// DefaultParams is argon2id at 19 MiB, two iterations, one lane.
//
// Memory is the interesting parameter and here it is bounded by deployment
// rather than by cryptography. The server runs in about 128Mi, and
// verification runs *in that process* on every login attempt, so the 64 MiB
// setting some guides recommend would commit half the container's memory to a
// single request and put two concurrent logins within reach of an OOM kill.
// 19 MiB clears the recommended floor with room to spare on a modest box.
//
// The parameters are recorded in every hash, so raising them later only affects
// passwords set after the change — existing logins keep working. See Verify.
func DefaultParams() Params {
	return Params{
		Memory:      19 * 1024,
		Iterations:  2,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// Hash returns a PHC-encoded argon2id hash of password at the default cost.
func Hash(password string) (string, error) { return HashWith(DefaultParams(), password) }

// HashWith is Hash at an explicit cost, for tests that cannot afford 19 MiB per
// call and for a future cost increase.
func HashWith(p Params, password string) (string, error) {
	if password == "" {
		return "", errors.New("password must not be empty")
	}

	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)

	// The PHC string format, which is what every other argon2 implementation
	// reads. Storing the parameters inline rather than assuming DefaultParams is
	// what makes a cost increase a non-event instead of a lockout.
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify reports whether password matches the encoded hash.
//
// The cost parameters are read from the hash, not from DefaultParams, so a
// password set under older parameters still verifies.
func Verify(encoded, password string) (bool, error) {
	p, salt, want, err := parse(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)

	// Constant time, so that the comparison does not leak how much of the hash
	// matched. The work above already dominates the timing signal, but a
	// byte-wise compare here would be a gratuitous one.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether a hash was produced at a weaker cost than the
// current default, so a caller that has the cleartext in hand — a successful
// login — can transparently upgrade it.
//
// Nothing calls this yet. It exists because the information is only available
// here, and a future cost increase without it would leave every existing
// password at the old cost forever.
func NeedsRehash(encoded string) (bool, error) {
	p, _, _, err := parse(encoded)
	if err != nil {
		return false, err
	}
	d := DefaultParams()
	return p.Memory < d.Memory ||
		p.Iterations < d.Iterations ||
		p.KeyLength < d.KeyLength, nil
}

func parse(encoded string) (Params, []byte, []byte, error) {
	// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return Params{}, nil, nil, fmt.Errorf("%w: expected 5 fields, got %d", ErrMalformedHash, len(parts)-1)
	}
	if parts[1] != "argon2id" {
		return Params{}, nil, nil, fmt.Errorf("%w: algorithm is %q, want argon2id", ErrMalformedHash, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: unreadable version %q", ErrMalformedHash, parts[2])
	}
	if version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("%w: argon2 version %d, want %d", ErrMalformedHash, version, argon2.Version)
	}

	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: unreadable parameters %q", ErrMalformedHash, parts[3])
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: undecodable salt", ErrMalformedHash)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: undecodable key", ErrMalformedHash)
	}
	// Bounded before the conversions below, which makes them provably safe and
	// stops a hand-edited row from asking argon2 for an absurd key length. Real
	// values are 16 and 32 bytes; the ceiling is generous rather than tight.
	const maxField = 1024
	if len(salt) == 0 || len(salt) > maxField {
		return Params{}, nil, nil, fmt.Errorf("%w: salt is %d bytes, want 1 to %d", ErrMalformedHash, len(salt), maxField)
	}
	if len(key) == 0 || len(key) > maxField {
		return Params{}, nil, nil, fmt.Errorf("%w: key is %d bytes, want 1 to %d", ErrMalformedHash, len(key), maxField)
	}

	p.SaltLength = uint32(len(salt)) //nolint:gosec // bounded by maxField immediately above
	p.KeyLength = uint32(len(key))   //nolint:gosec // bounded by maxField immediately above
	return p, salt, key, nil
}

// FeverAPIKey is MD5 of "username:password", which is the credential the Fever
// protocol specifies.
//
// MD5 is not a choice made here — it is the wire format every Fever client
// implements, and deviating from it would mean implementing a different
// protocol. It never protects the password: that is argon2id above. This value
// is only ever compared against what a client presents, and it must be computed
// while the cleartext exists because it cannot be derived from the hash.
func FeverAPIKey(username, password string) string {
	sum := md5.Sum([]byte(username + ":" + password)) //nolint:gosec // Fever wire format; see doc comment
	return hex.EncodeToString(sum[:])
}
