package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Errors an administrator's action can produce that are not faults.
var (
	// ErrUsernameBlank means the name was empty or only whitespace.
	ErrUsernameBlank = errors.New("a username is required")

	// ErrUsernameTaken means another account already has that name.
	ErrUsernameTaken = errors.New("that username is taken")

	// ErrUsernameInvalid means the name contains something a username may not.
	ErrUsernameInvalid = errors.New("that username contains something it may not")

	// ErrInvalidRole means the role was neither admin nor reader.
	ErrInvalidRole = errors.New("that is not a role")

	// ErrLastAdmin means the deletion would leave the archive with no
	// administrator, and therefore no way to make another one.
	ErrLastAdmin = errors.New("that is the only administrator")

	// ErrLinkUnusable means a setup link is unknown, expired, or already spent.
	// One error for all three deliberately — see RedeemSetupLink.
	ErrLinkUnusable = errors.New("that link is no longer usable")
)

// MaxUsernameLength bounds a username. Generous, and a bound rather than a
// judgement: the column is text, and something has to stop a megabyte of form
// field becoming a row.
const MaxUsernameLength = 64

// SetupLinkTTL is how long a setup link stays usable.
//
// Days rather than hours because there is no mail here: the link is handed over
// out of band — read out, messaged, written down — and a household member may not
// sit down at a browser today. Long enough to be practical, short enough that a
// forgotten link in a chat log stops working.
const SetupLinkTTL = 7 * 24 * time.Hour

// ValidUsername cleans and checks a username, returning the form to store.
//
// Whitespace inside is refused rather than collapsed. A name with a space is
// usually a typo or a paste, and silently storing "jane  doe" gives somebody an
// account they cannot type the name of. Control characters are refused because
// this ends up in HTML, logs and a CLI argument.
func ValidUsername(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", ErrUsernameBlank
	case len(name) > MaxUsernameLength:
		return "", ErrUsernameInvalid
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == ' ' || r == '\t' {
			return "", ErrUsernameInvalid
		}
	}
	return name, nil
}

// ValidRole checks a role against the two the database allows.
//
// Taken from a fixed set rather than trusted from a form, so a hand-crafted POST
// cannot invent a role — which would be refused by the check constraint anyway,
// but as a database error rather than as an answer.
func ValidRole(role string) (string, error) {
	switch role {
	case RoleAdmin, RoleReader:
		return role, nil
	}
	return "", ErrInvalidRole
}

// CreateUser adds an account with no password.
//
// No password, always: the two ways to get one are an administrator running
// `tome user passwd`, or a setup link that lets the reader choose it themselves.
// Neither belongs in the act of creating the account, and an account created with
// a password an administrator picked is one the administrator knows.
func (s *SystemStore) CreateUser(ctx context.Context, username, role string) (UserID, error) {
	username, err := ValidUsername(username)
	if err != nil {
		return 0, err
	}
	role, err = ValidRole(role)
	if err != nil {
		return 0, err
	}

	var id UserID
	err = s.pool.QueryRow(ctx,
		`INSERT INTO users (username, role) VALUES ($1, $2) RETURNING id`,
		username, role,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrUsernameTaken
		}
		return 0, fmt.Errorf("creating user %q: %w", username, err)
	}
	return id, nil
}

// AccountSummary is one row of the account list.
type AccountSummary struct {
	Account

	// HasPassword says whether this account can be signed in to at all. An
	// account without one is waiting for a setup link to be redeemed, which is
	// the state every new account starts in.
	HasPassword bool

	// Feeds is how many subscriptions this reader has, which is what makes
	// deleting an account a decision rather than a click.
	Feeds int

	// PendingLink is when this account's outstanding setup link expires, or nil
	// if there is none waiting.
	PendingLink *time.Time
}

// ListAccounts returns every account, for the administration page.
func (s *SystemStore) ListAccounts(ctx context.Context) ([]AccountSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.username, u.password_hash, u.role, u.session_epoch,
		       (SELECT count(*) FROM feeds f WHERE f.user_id = u.id),
		       (SELECT max(l.expires_at) FROM password_setup_links l
		         WHERE l.user_id = u.id AND l.used_at IS NULL AND l.expires_at > now())
		FROM users u
		ORDER BY u.username`)
	if err != nil {
		return nil, fmt.Errorf("listing accounts: %w", err)
	}
	defer rows.Close()

	var out []AccountSummary
	for rows.Next() {
		var (
			a    AccountSummary
			hash string
		)
		if err := rows.Scan(&a.ID, &a.Username, &hash, &a.Role, &a.SessionEpoch,
			&a.Feeds, &a.PendingLink); err != nil {
			return nil, fmt.Errorf("scanning account: %w", err)
		}
		// The hash itself never leaves the store. Whether there is one does.
		a.HasPassword = hash != ""
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetRole changes what an account may administer.
//
// Guarded by the same last-administrator rule as deletion, and for the same
// reason: an archive whose only admin demotes themselves has no way back short of
// the command line.
func (s *SystemStore) SetRole(ctx context.Context, id UserID, role string) error {
	role, err := ValidRole(role)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if role != RoleAdmin {
		last, err := lastAdmin(ctx, tx, id)
		if err != nil {
			return err
		}
		if last {
			return ErrLastAdmin
		}
	}

	tag, err := tx.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, id, role)
	if err != nil {
		return fmt.Errorf("setting the role of user %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return tx.Commit(ctx)
}

// DeleteUser removes an account, its subscriptions and everything it read.
//
// The articles and images stay. Every scoped table cascades from users, and
// nothing an article is made of belongs to a reader — which is why a household
// can lose a member without losing the archive. What is left behind is articles
// nothing references any more, which is `tome prune`'s case rather than this one:
// deleting them here would delete pages other readers may still hold state for.
func (s *SystemStore) DeleteUser(ctx context.Context, id UserID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	last, err := lastAdmin(ctx, tx, id)
	if err != nil {
		return err
	}
	if last {
		// Refused rather than allowed-with-a-warning. An archive with no
		// administrator cannot make one through the interface, and the remedy is a
		// hand-written UPDATE on the database.
		return ErrLastAdmin
	}

	tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("deleting user %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return tx.Commit(ctx)
}

// lastAdmin reports whether id is an administrator and the only one.
//
// Inside the caller's transaction, so the count and the change that depends on it
// cannot be separated by another administrator being removed in between.
func lastAdmin(ctx context.Context, tx pgx.Tx, id UserID) (bool, error) {
	var last bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND role = $2)
		   AND (SELECT count(*) FROM users WHERE role = $2) = 1`,
		id, RoleAdmin,
	).Scan(&last)
	if err != nil {
		return false, fmt.Errorf("counting administrators: %w", err)
	}
	return last, nil
}

// SetupLink is a link that has been issued, as far as the issuer needs to know.
type SetupLink struct {
	// Token is the secret, and this is the only moment it exists in readable
	// form: the database keeps a hash. An issuer that loses it has to issue
	// another.
	Token     string
	ExpiresAt time.Time
}

// IssueSetupLink creates a single-use link for setting this account's password.
//
// Any earlier unused link for the same account is spent in the same transaction.
// One outstanding link at a time is a smaller thing to reason about than several,
// and re-issuing usually means the first one went astray — in which case it
// should stop working.
func (s *SystemStore) IssueSetupLink(ctx context.Context, id UserID) (SetupLink, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return SetupLink{}, fmt.Errorf("generating a setup token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(SetupLinkTTL)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SetupLink{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The account has to exist, and saying so here means the caller gets
	// "no such user" rather than a foreign key violation.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, id).Scan(&exists); err != nil {
		return SetupLink{}, fmt.Errorf("checking user %d: %w", id, err)
	}
	if !exists {
		return SetupLink{}, pgx.ErrNoRows
	}

	if _, err := tx.Exec(ctx,
		`UPDATE password_setup_links SET used_at = now()
		 WHERE user_id = $1 AND used_at IS NULL`, id); err != nil {
		return SetupLink{}, fmt.Errorf("superseding earlier links for user %d: %w", id, err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO password_setup_links (user_id, token_sha256, expires_at) VALUES ($1, $2, $3)`,
		id, hashToken(token), expires); err != nil {
		return SetupLink{}, fmt.Errorf("issuing a setup link for user %d: %w", id, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return SetupLink{}, err
	}
	return SetupLink{Token: token, ExpiresAt: expires}, nil
}

// SetupLinkAccount returns the account a usable link belongs to, without spending
// it.
//
// The page that asks for a new password needs to greet somebody by name before
// they have typed anything, and it should not offer the form at all if the link
// is spent. Redemption checks again inside its own transaction, so this is
// convenience rather than the guard.
func (s *SystemStore) SetupLinkAccount(ctx context.Context, token string) (Account, error) {
	var a Account
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.username, u.password_hash, u.role, u.session_epoch
		FROM password_setup_links l JOIN users u ON u.id = l.user_id
		WHERE l.token_sha256 = $1 AND l.used_at IS NULL AND l.expires_at > now()`,
		hashToken(token),
	).Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Role, &a.SessionEpoch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrLinkUnusable
		}
		return Account{}, fmt.Errorf("looking up a setup link: %w", err)
	}
	return a, nil
}

// RedeemSetupLink spends a link and sets the password it was issued for.
//
// One transaction, and the ordering is the point: the link is marked used by an
// UPDATE that only matches an unused, unexpired row, and the password is written
// only if that UPDATE matched. Two people racing the same link therefore produce
// one password change and one refusal, rather than two writes where the second
// silently wins.
//
// It bumps session_epoch like any other password change, which matters most here:
// a reset is exactly the case where somebody else may be holding a live session.
//
// Unknown, expired and already-spent all return ErrLinkUnusable. The distinction
// would be of no use to whoever holds the link and of some use to whoever does
// not, and the sign-in page already sets that precedent.
func (s *SystemStore) RedeemSetupLink(ctx context.Context, token, hash, apiKey string) (Account, error) {
	if hash == "" {
		return Account{}, fmt.Errorf("password hash must not be empty")
	}
	if apiKey == "" {
		return Account{}, fmt.Errorf("fever api key must not be empty")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Account{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID UserID
	err = tx.QueryRow(ctx, `
		UPDATE password_setup_links SET used_at = now()
		WHERE token_sha256 = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING user_id`, hashToken(token)).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrLinkUnusable
		}
		return Account{}, fmt.Errorf("redeeming a setup link: %w", err)
	}

	var a Account
	err = tx.QueryRow(ctx, `
		UPDATE users
		SET password_hash = $2, api_key = $3, session_epoch = session_epoch + 1
		WHERE id = $1
		RETURNING `+accountColumns, userID, hash, apiKey,
	).Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Role, &a.SessionEpoch)
	if err != nil {
		return Account{}, fmt.Errorf("setting the password for user %d: %w", userID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Account{}, err
	}
	return a, nil
}

// hashToken is what the database stores in place of a token.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SetUsername renames an account and rewrites its Fever API key.
//
// The two are one operation because of how the Fever protocol works: the key is
// md5(username:password), computed by the *client* from what the reader types. Change
// the name without the key and every mobile client silently stops authenticating,
// with a stored key that no longer corresponds to any credential anybody holds.
//
// The key cannot be recomputed from the stored hash — that is the whole point of an
// argon2 hash — so a rename needs the cleartext password. That is why the form asks
// for it, and it is a reasonable thing to ask for anyway: a rename changes how the
// reader signs in.
func (s *SystemStore) SetUsername(ctx context.Context, id UserID, username, apiKey string) error {
	username, err := ValidUsername(username)
	if err != nil {
		return err
	}
	if apiKey == "" {
		return fmt.Errorf("fever api key must not be empty")
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET username = $2, api_key = $3 WHERE id = $1`, id, username, apiKey)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrUsernameTaken
		}
		return fmt.Errorf("renaming user %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// SetRetention stores how long a reader keeps what they have read.
//
// nil means "follow the archive's own setting"; zero is a real value meaning keep
// everything, and is deliberately distinct from nil.
func (s *SystemStore) SetRetention(ctx context.Context, id UserID, d *time.Duration) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET retain_after_read = $2 WHERE id = $1`, id, d)
	if err != nil {
		return fmt.Errorf("setting retention for user %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
