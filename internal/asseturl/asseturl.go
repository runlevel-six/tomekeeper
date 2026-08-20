// Package asseturl signs the archived-image URLs that leave this service.
//
// The web reader needs none of this: it fetches images with the reader's session
// cookie, which is what `/assets/` has always required. This exists for the one
// caller that has a body but no cookie — the Fever API, whose clients
// authenticate with an api_key in a POST body and then render the article's HTML
// in their own view. An `<img>` tag cannot carry that credential, so without
// something in the URL itself every picture in every mobile client is a broken
// image icon.
//
// The signature is the credential, which is the same answer Miniflux reaches for
// the same problem (`/proxy/<hmac>/<url>`), and it keeps `/assets/` closed to a
// request that merely guessed a path. Three properties are worth stating because
// each one is a decision:
//
//   - The MAC covers the path *and* the expiry together, so a signature cannot be
//     lifted from one image onto another, and the expiry cannot be extended
//     without invalidating the signature. A MAC over the path alone would be a
//     permanent bearer token for that image; over the expiry alone, a permanent
//     one for the whole archive.
//   - The key is derived from TOME_SESSION_KEY rather than configured separately.
//     One secret to generate and one to rotate, and the session package's own
//     comment anticipates this: the HKDF info label differs, so the two keys are
//     independent even though the secret is shared. A consequence to know rather
//     than discover: rotating that secret invalidates outstanding image URLs along
//     with every session.
//   - There is an expiry at all, unlike Miniflux, whose proxy URLs are valid until
//     the process restarts. A URL that escapes into a log or a Referer header
//     stops working on its own here.
package asseturl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

// DefaultTTL is how long a signed image URL stays valid.
//
// Bounded by how a Fever client behaves rather than by taste. A client syncs
// bodies and renders them later — sometimes days later, on a phone that was in a
// pocket — and an image that 403s because the sync was on Tuesday is
// indistinguishable to the reader from an image the archive never had. Thirty days
// covers that with room to spare while still being a window rather than forever.
//
// It is not configurable, deliberately: the value only matters in the gap between
// "long enough to read what you synced" and "short enough to be a window", and
// every number in that range behaves identically. See the package comment for what
// rotating the secret does.
const DefaultTTL = 30 * 24 * time.Hour

// keyLength is the HMAC key size: one SHA-256 block's worth of output from HKDF.
const keyLength = 32

// SignatureParam is the single query parameter a signed URL carries.
//
// Exported because the handler that verifies it and the code that builds it must
// agree, and a string literal in two files is how they stop agreeing.
//
// One parameter rather than a separate expiry and signature, and the reason is where
// these URLs live. They are written into an HTML attribute, so an ampersand between
// two parameters is correctly serialized as `&amp;` — which every conforming parser
// turns back into `&`, and which some decade-old mobile client eventually will not.
// Carrying the expiry inside the value means the URL contains no character that HTML
// escaping touches, so what the client fetches is byte for byte what was signed.
const SignatureParam = "sig"

// signatureSeparator divides the expiry from the MAC inside that one value.
//
// A dot, because it is safe unescaped in a query string and cannot appear in either
// half: the expiry is digits and the MAC is base64url, whose alphabet excludes it.
const signatureSeparator = "."

// Signer signs and verifies asset paths.
type Signer struct {
	key []byte
	ttl time.Duration
}

// NewSigner derives a signing key from the same secret sessions are sealed with.
//
// secret may be any length; HKDF stretches it. As with sessions, that does not
// manufacture entropy — a short secret is still a weak secret.
func NewSigner(secret []byte, ttl time.Duration) (*Signer, error) {
	if len(secret) == 0 {
		return nil, errors.New("asset signing secret must not be empty")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("asset URL ttl must be positive, got %s", ttl)
	}

	key := make([]byte, keyLength)
	// A different info label from the session cipher's, which is what makes this an
	// independent key rather than the same one wearing a hat.
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, nil, []byte("tomekeeper asset url v1")), key); err != nil {
		return nil, fmt.Errorf("deriving the asset signing key: %w", err)
	}
	return &Signer{key: key, ttl: ttl}, nil
}

// Sign returns path with an expiry and a signature appended.
//
// path is taken as an absolute request path — "/assets/sha256/a1/b2/….avif" — and
// is returned unchanged if it already carries a query, because that would mean the
// caller is signing something this was not written for.
func (s *Signer) Sign(path string) string {
	if path == "" || strings.ContainsAny(path, "?#") {
		return path
	}

	expiry := time.Now().Add(s.ttl).Unix()
	value := strconv.FormatInt(expiry, 10) + signatureSeparator + s.signature(path, expiry)

	return path + "?" + SignatureParam + "=" + value
}

// Verify reports whether sig authorizes this path right now.
//
// A request with no signature at all is the web reader's, which is authorized by its
// session instead. The caller decides that; this only answers the signature question.
func (s *Signer) Verify(path, sig string) bool {
	if path == "" || sig == "" {
		return false
	}

	exp, mac, found := strings.Cut(sig, signatureSeparator)
	if !found {
		return false
	}

	expiry, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		return false
	}

	// The signature is checked before the clock, and both are checked every time.
	// Checking expiry first would be a small oracle — a caller could learn whether a
	// timestamp was in range without holding a valid signature — and returning early
	// on a stale-but-correctly-signed URL would still be a refusal, so there is
	// nothing to gain from the shortcut.
	want := s.signature(path, expiry)
	if !hmac.Equal([]byte(mac), []byte(want)) {
		return false
	}
	return time.Now().Unix() < expiry
}

// signature is the MAC over one path and one expiry.
//
// The two are joined by a byte that cannot appear in either — a newline, given an
// expiry is digits and a request path cannot contain one — so that no pair of
// (path, expiry) values can produce the same message as a different pair. Without a
// delimiter that holds, "/a/b" at 12 and "/a/b1" at 2 sign the same bytes.
func (s *Signer) signature(path string, expiry int64) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(path))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(strconv.FormatInt(expiry, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
