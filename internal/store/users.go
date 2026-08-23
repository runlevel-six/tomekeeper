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

// EnsureSeedUser creates the operator's account if it does not exist, and returns
// its id and the name it actually has.
//
// Idempotent, so running `tome migrate` on every deploy is safe.
//
// **It no longer renames an existing account, and that was a bug with teeth.** The
// upsert used to carry `SET username = EXCLUDED.username`, so `tome migrate` — which
// runs as a Job on every single deploy — wrote TOME_USERNAME over whatever the account
// was called. That was harmless while configuration was the only thing that could name
// an account. It stopped being harmless when a reader could rename themselves from
// Settings: the rename worked, survived a sign-out, and was silently undone by the
// next release. Reported from production after v0.17.0, on an account renamed two
// commits earlier.
//
// Worse than losing the name, it left the account inconsistent. Renaming rewrites the
// Fever API key, because that key is md5(username:password) and is computed by the
// client — so after the reset the stored key still matched the *old* name, mobile
// clients kept working under a username the web interface no longer knew, and a
// client configured afresh with the reset name would have been refused.
//
// The same lesson had already been learned one field over: setting the password here
// unconditionally used to revoke sessions on every deploy, and the fix was to verify
// first and write nothing when nothing changed. Configuration seeds an archive; it
// does not get to overrule what the people using it have since chosen.
//
// TOME_USERNAME therefore names the account **at creation**. Changing it later does
// nothing, and the returned name is what the account is really called so the caller
// can say so rather than reporting the configured value back at whoever set it.
//
// The password hash is left empty. Authentication arrives with the web interface; there is no
// login surface to protect until there is a login.
func (s *SystemStore) EnsureSeedUser(ctx context.Context, username string) (UserID, string, error) {
	if username == "" {
		return 0, "", fmt.Errorf("username must not be empty")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Seeded as an admin, because this account *is* the operator: it is created
	// from configuration by `tome migrate`, and there is nobody else to grant it
	// anything. Leaving it at the column default produced an archive whose only
	// account could not reach the controls that administer it — on a fresh install
	// only, since 00015 promotes the accounts that already exist, which is exactly
	// the shape of bug that survives because the upgrade path is the one anybody
	// tests.
	//
	// The conflict branch deliberately touches nothing: not the role, so re-running
	// `tome migrate` cannot re-promote an account somebody demoted on purpose, and not
	// the username, for the reason above. DO UPDATE with no assignment is not
	// expressible, so the row is re-read instead — which is also how the caller learns
	// the name the account really has.
	var (
		id   UserID
		name string
	)
	err = tx.QueryRow(ctx, `
		WITH seeded AS (
			INSERT INTO users (id, username, role)
			VALUES ($1, $2, $3)
			ON CONFLICT (id) DO NOTHING
			RETURNING id, username
		)
		SELECT id, username FROM seeded
		UNION ALL
		SELECT id, username FROM users WHERE id = $1
		LIMIT 1`,
		SeedUserID, username, RoleAdmin,
	).Scan(&id, &name)
	if err != nil {
		return 0, "", fmt.Errorf("seeding user: %w", err)
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
		return 0, "", fmt.Errorf("resetting user id sequence: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, "", err
	}
	return id, name, nil
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
//
// It also bumps session_epoch, for the same reason and with the same force: a
// password change that left existing browser sessions signed in would be no
// change at all to whoever already had one. So this is the wrong method for
// re-hashing an unchanged password at stronger parameters — that is not a
// credential change and must not sign anybody out. Nothing does that yet;
// whatever does should write the hash without touching the epoch.
func (s *SystemStore) SetPassword(ctx context.Context, id UserID, hash, apiKey string) error {
	if hash == "" {
		return fmt.Errorf("password hash must not be empty")
	}
	if apiKey == "" {
		return fmt.Errorf("fever api key must not be empty")
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE users
		 SET password_hash = $2, api_key = $3, session_epoch = session_epoch + 1
		 WHERE id = $1`,
		id, hash, apiKey)
	if err != nil {
		return fmt.Errorf("setting password for user %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("setting password: no user with id %d", id)
	}
	return nil
}

// Role names a level of privilege.
//
// Two values, and the database has a matching constraint. The distinction is not
// "who may read what" — reading is scoped per user by construction — but "who may
// change things every reader shares": domain rules, retention, the archive-wide
// audit, and other people's accounts.
const (
	RoleAdmin  = "admin"
	RoleReader = "reader"
)

// Account is one user's row, as far as authentication and privilege are
// concerned. It deliberately carries no reading preferences; those are
// Preferences, loaded by the handlers that render a page.
type Account struct {
	ID           UserID
	Username     string
	PasswordHash string
	Role         string

	// SessionEpoch is compared against the epoch sealed into a credential. A
	// credential carrying anything else has been revoked.
	SessionEpoch int64
}

// IsAdmin reports whether this account may change what other readers share.
func (a Account) IsAdmin() bool { return a.Role == RoleAdmin }

const accountColumns = `id, username, password_hash, role, session_epoch`

// Credentials returns the account for a username.
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
func (s *SystemStore) Credentials(ctx context.Context, username string) (Account, error) {
	var a Account
	err := s.pool.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM users WHERE username = $1`, username,
	).Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Role, &a.SessionEpoch)
	if err != nil {
		return Account{}, fmt.Errorf("looking up credentials for %q: %w", username, err)
	}
	return a, nil
}

// SessionUser returns the account a credential names.
//
// This is what turns an authentic cookie into a signed-in reader, and it exists
// because a credential is a claim about a moment that has passed. Between issuing
// one and presenting it the account may have been deleted, or had its sessions
// revoked; neither is visible in the cookie, which is why the cookie alone is not
// enough to admit a request.
//
// On SystemStore for the same reason as Credentials: there is no user to scope to
// until this returns.
func (s *SystemStore) SessionUser(ctx context.Context, id UserID) (Account, error) {
	var a Account
	err := s.pool.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM users WHERE id = $1`, id,
	).Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Role, &a.SessionEpoch)
	if err != nil {
		return Account{}, fmt.Errorf("looking up user %d: %w", id, err)
	}
	return a, nil
}

// BumpSessionEpoch invalidates every outstanding session for one user and
// returns the new epoch.
//
// Called wherever a reader's existing credentials should stop working: a password
// change, an explicit sign-out-everywhere, and — though the cascade makes it moot
// — before deleting an account. Returns the new value so a caller that is also
// issuing a fresh credential in the same request can seal the current epoch
// rather than the one it just superseded, which would sign the reader out of the
// session they were in the middle of creating.
func (s *SystemStore) BumpSessionEpoch(ctx context.Context, id UserID) (int64, error) {
	var epoch int64
	err := s.pool.QueryRow(ctx,
		`UPDATE users SET session_epoch = session_epoch + 1 WHERE id = $1 RETURNING session_epoch`,
		id,
	).Scan(&epoch)
	if err != nil {
		return 0, fmt.Errorf("revoking sessions for user %d: %w", id, err)
	}
	return epoch, nil
}

// AnyPasswordSet reports whether any account in this archive can be signed in to.
//
// The sign-in page uses it to tell a first-run operator that no password exists
// yet, which is otherwise indistinguishable from a forgotten one and has an
// entirely different fix. Asked about the archive rather than about a named
// account on purpose: the page has no username to ask about before the form is
// submitted, and answering per-account would tell an anonymous visitor which
// names exist.
func (s *SystemStore) AnyPasswordSet(ctx context.Context) (bool, error) {
	var any bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE password_hash <> '')`,
	).Scan(&any)
	if err != nil {
		return false, fmt.Errorf("checking whether any password is set: %w", err)
	}
	return any, nil
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
