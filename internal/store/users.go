package store

import (
	"context"
	"fmt"
)

// SeedUserID is the id of the single v1 user.
//
// Multi-user is a later milestone; until then this is the only user in the
// system, and it is created by `tome migrate` from configuration.
const SeedUserID UserID = 1

// EnsureSeedUser creates or renames the single v1 user and returns its id.
//
// This is idempotent, so running `tome migrate` repeatedly is safe, and it
// tracks TOME_USERNAME: changing the configured name renames the existing user
// rather than creating a second one, which would orphan every feed.
//
// The password hash is left empty. Authentication arrives with the web interface; there is no
// login surface to protect until there is a login.
func (s *SystemStore) EnsureSeedUser(ctx context.Context, username string) (UserID, error) {
	if username == "" {
		return 0, fmt.Errorf("username must not be empty")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id UserID
	err = tx.QueryRow(ctx, `
		INSERT INTO users (id, username)
		VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET username = EXCLUDED.username
		RETURNING id`,
		SeedUserID, username,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("seeding user: %w", err)
	}

	// Inserting an explicit id does not advance the bigserial sequence, so
	// without this the first user created by a future signup flow would collide
	// with the seed user. Fixing it here costs nothing; discovering it in two
	// years costs an afternoon.
	if _, err := tx.Exec(ctx, `
		SELECT setval(
			pg_get_serial_sequence('users', 'id'),
			GREATEST((SELECT max(id) FROM users), 1))`,
	); err != nil {
		return 0, fmt.Errorf("resetting user id sequence: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return id, nil
}

// SetPassword stores a password hash and its matching Fever API key.
//
// Both in one statement, deliberately. The Fever API design requires the Fever key to be written
// whenever the password is set, because MD5 of the cleartext cannot be recovered
// from an argon2 hash later. Two separate updates could leave the pair
// inconsistent — a hash from the new password beside a key from the old one —
// and the symptom would be Fever clients silently authenticating with a password
// that no longer works, with nothing anywhere to explain it.
//
// Setting a password therefore always disconnects existing Fever clients. That is
// correct rather than unfortunate: the key *is* the credential, so rotating one
// must rotate the other.
func (s *SystemStore) SetPassword(ctx context.Context, id UserID, hash, apiKey string) error {
	if hash == "" {
		return fmt.Errorf("password hash must not be empty")
	}
	if apiKey == "" {
		return fmt.Errorf("fever api key must not be empty")
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET password_hash = $2, api_key = $3 WHERE id = $1`,
		id, hash, apiKey)
	if err != nil {
		return fmt.Errorf("setting password for user %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("setting password: no user with id %d", id)
	}
	return nil
}

// Credentials returns the id and stored password hash for a username.
//
// On SystemStore rather than Store because a login has no user to scope to yet —
// resolving the username *is* the operation. The scoping discipline puts every cross-user query
// here so that the exceptions are greppable rather than accidental.
//
// A user with no password set yet returns an empty hash and no error. Callers
// must treat that as "login is not possible" rather than as a hash to verify
// against: an empty hash cannot be produced by Hash, so any password would fail,
// but failing for the wrong stated reason sends the operator hunting for a typo
// instead of running `tome migrate` with TOME_PASSWORD set.
func (s *SystemStore) Credentials(ctx context.Context, username string) (UserID, string, error) {
	var (
		id   UserID
		hash string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, password_hash FROM users WHERE username = $1`, username,
	).Scan(&id, &hash)
	if err != nil {
		return 0, "", fmt.Errorf("looking up credentials for %q: %w", username, err)
	}
	return id, hash, nil
}

// LookupUser returns the id of the user with the given username.
func (s *SystemStore) LookupUser(ctx context.Context, username string) (UserID, error) {
	var id UserID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE username = $1`, username,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("looking up user %q: %w", username, err)
	}
	return id, nil
}
