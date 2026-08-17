package store

import (
	"context"
	"fmt"
)

// SeedUserID is the id of the single v1 user. Multi-user is a later milestone
// (M9); until then this is the only user in the system, and it is created by
// `tome migrate` from configuration.
const SeedUserID UserID = 1

// EnsureSeedUser creates or renames the single v1 user and returns its id.
//
// This is idempotent, so running `tome migrate` repeatedly is safe, and it
// tracks TOME_USERNAME: changing the configured name renames the existing user
// rather than creating a second one, which would orphan every feed.
//
// The password hash is left empty. Authentication arrives with M4; there is no
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
	// without this the first user created by M9's signup flow would collide
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
