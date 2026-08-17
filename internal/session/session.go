// Package session issues and reads the credential that identifies a signed-in
// user.
//
// There is exactly one implementation, a signed and encrypted cookie, chosen
// because a single-user reader does not need revocation and a cookie needs no
// table, no cleanup, and no query per request. The interface exists anyway:
// revocation starts to matter the moment a second person has an account. Keeping
// handlers behind Store means adding a database-backed implementation later is a
// new type plus a migration, rather than an edit to every handler that currently
// reads a cookie inline.
package session

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// CookieName is the name of the session cookie.
const CookieName = "tome_session"

// DefaultTTL is how long a session lasts. Long, because this is a reading tool
// used in short visits over months, and being asked to sign in again every week
// is friction with no security benefit for a single-user archive.
const DefaultTTL = 30 * 24 * time.Hour

// KeyLength is the number of random bytes a session key should carry.
const KeyLength = 32

// Store issues, reads, and clears session credentials.
type Store interface {
	// Issue writes a credential identifying userID.
	Issue(w http.ResponseWriter, userID store.UserID) error

	// Identify returns the user the request is authenticated as. The boolean is
	// false for anything not positively identified — absent, expired, tampered
	// with, or encrypted under a different key — and callers must not
	// distinguish those cases to the client.
	Identify(r *http.Request) (store.UserID, bool)

	// Clear revokes the credential in the client.
	Clear(w http.ResponseWriter)
}

// GenerateKey returns a random session key.
//
// Used when none is configured. Sessions then survive only until the process
// restarts, which is why the caller is expected to say so out loud.
func GenerateKey() ([]byte, error) {
	key := make([]byte, KeyLength)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating a session key: %w", err)
	}
	return key, nil
}

// Cookie is a Store backed by an encrypted cookie.
//
// AES-GCM rather than sign-then-encrypt with two primitives: it is an AEAD, so
// one operation gives both confidentiality and integrity, and there is no way to
// accidentally verify a signature over the wrong bytes. The cookie is encrypted
// rather than merely signed so the contents are not a readable statement about
// which user id someone is — that is not a secret worth much, but it costs
// nothing to withhold.
type Cookie struct {
	aead   cipher.AEAD
	ttl    time.Duration
	secure bool
}

// NewCookie returns a Store that keeps sessions in an encrypted cookie.
//
// secret may be any length and any entropy; it is stretched to an AES-256 key
// with HKDF. That does not manufacture entropy, so a short secret is still a
// weak secret — the configuration reference says to generate a random one.
//
// secure controls the Secure attribute. It defaults on, which means the cookie
// is only sent over HTTPS. Browsers treat localhost as a secure context, so a
// local first run still works; a deployment serving plain HTTP on a LAN address
// has to turn it off deliberately.
func NewCookie(secret []byte, ttl time.Duration, secure bool) (*Cookie, error) {
	if len(secret) == 0 {
		return nil, errors.New("session secret must not be empty")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("session ttl must be positive, got %s", ttl)
	}

	// A fixed info label, so the same secret used for something else in future
	// derives a different key.
	key := make([]byte, KeyLength)
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, nil, []byte("tomekeeper session v1")), key); err != nil {
		return nil, fmt.Errorf("deriving the session key: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating the session cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating the session AEAD: %w", err)
	}

	return &Cookie{aead: aead, ttl: ttl, secure: secure}, nil
}

// payloadLen is the plaintext: eight bytes of user id, eight of expiry.
//
// Fixed-width binary rather than JSON, so there is nothing to parse loosely and
// no way for a decode to succeed on a payload of the wrong shape.
const payloadLen = 16

// Issue implements Store.
func (c *Cookie) Issue(w http.ResponseWriter, userID store.UserID) error {
	if userID <= 0 {
		return fmt.Errorf("refusing to issue a session for user id %d", userID)
	}

	expires := time.Now().Add(c.ttl)

	payload := make([]byte, payloadLen)
	binary.BigEndian.PutUint64(payload[0:8], uint64(userID)) //nolint:gosec // refused above unless positive
	binary.BigEndian.PutUint64(payload[8:16], uint64(expires.Unix()))

	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generating a session nonce: %w", err)
	}

	sealed := c.aead.Seal(nil, nonce, payload, nil)
	value := base64.RawURLEncoding.EncodeToString(append(nonce, sealed...))

	// G124 wants a literal Secure: true. It is a configured value here on purpose
	// — see NewCookie — while HttpOnly and SameSite are set unconditionally.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is configurable by design
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(c.ttl.Seconds()),
		HttpOnly: true,
		Secure:   c.secure,
		// Lax rather than Strict: Strict drops the cookie on any cross-site
		// navigation, so following a link to your own archive from anywhere else
		// would land on a login page. Lax still refuses cross-site POSTs, which
		// is the part that matters for state-changing requests.
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// Identify implements Store.
func (c *Cookie) Identify(r *http.Request) (store.UserID, bool) {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return 0, false
	}

	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return 0, false
	}

	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize+c.aead.Overhead()+payloadLen {
		return 0, false
	}

	// Open authenticates as it decrypts, so a tampered or foreign cookie fails
	// here rather than producing a plausible user id.
	payload, err := c.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil || len(payload) != payloadLen {
		return 0, false
	}

	rawUser := binary.BigEndian.Uint64(payload[0:8])
	rawExpires := binary.BigEndian.Uint64(payload[8:16])

	// Bounded before converting to signed types. These bytes are authenticated, so
	// an out-of-range value means our own encoder wrote something impossible rather
	// than an attacker having succeeded — but an unchecked conversion would wrap,
	// and a wrapped user id that lands on a positive number is worth refusing
	// outright rather than reasoning about.
	if rawUser == 0 || rawUser > math.MaxInt64 || rawExpires > math.MaxInt64 {
		return 0, false
	}

	// Expiry is checked here as well as being set on the cookie, because the
	// cookie's own expiry is the client's to ignore.
	if time.Now().Unix() >= int64(rawExpires) { //nolint:gosec // bounded above
		return 0, false
	}
	return store.UserID(rawUser), true //nolint:gosec // bounded above
}

// Clear implements Store.
func (c *Cookie) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is configurable by design
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   c.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

var _ Store = (*Cookie)(nil)
