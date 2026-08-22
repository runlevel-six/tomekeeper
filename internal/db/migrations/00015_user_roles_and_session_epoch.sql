-- Gives users a privilege level and a way to have their sessions revoked.
--
-- Migrations are append-only. 00001 through 00014 have run.

-- +goose Up

-- Two roles, not a boolean. `is_admin` reads fine until there is a third kind of
-- account, at which point the column cannot carry it and every query naming it has
-- to change; a name can take a new value with a constraint edit. The column is also
-- read in template conditionals, where `.User.Role` says more than `.User.IsAdmin`
-- negated.
--
-- The constraint is deliberate rather than leaving the column free text: a typo in a
-- role name would otherwise silently produce an account with no privileges, which
-- looks like a permissions bug and is a data-entry one.
ALTER TABLE users ADD COLUMN IF NOT EXISTS role text NOT NULL DEFAULT 'reader';

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check CHECK (role IN ('admin', 'reader'));

-- Every user that exists when this runs is the operator. There is no way to create a
-- second account before this migration — the only account is the one `tome migrate`
-- seeds from configuration — so this promotes exactly one row, and doing it by
-- predicate rather than by id keeps it correct for an install whose seed user is not
-- id 1 for some reason nobody has thought of yet.
UPDATE users SET role = 'admin';

-- The session epoch is what makes a cookie revocable without a sessions table.
--
-- The value is sealed into the credential when it is issued and compared on every
-- request; a cookie whose epoch is behind the user's is refused. Bumping this column
-- therefore invalidates every outstanding session for that reader at once, which is
-- what "delete this account", "change my password" and "sign out everywhere" all
-- need.
--
-- It buys revocation-per-user, not revocation-per-device. That is the whole trade
-- against a sessions table: one column and no query beyond the row the request
-- already has to read, in exchange for not being able to sign out one phone while
-- leaving a laptop signed in. The session interface is unchanged by this, so a
-- table-backed implementation stays available if per-device revocation is ever
-- wanted for its own sake.
ALTER TABLE users ADD COLUMN IF NOT EXISTS session_epoch bigint NOT NULL DEFAULT 0;

-- +goose Down

-- Dropping these signs everybody out, which is correct rather than unfortunate: a
-- binary old enough to want this schema does not read the epoch, so every credential
-- it would accept is one this schema was issued to revoke.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users DROP COLUMN IF EXISTS session_epoch;
ALTER TABLE users DROP COLUMN IF EXISTS role;
