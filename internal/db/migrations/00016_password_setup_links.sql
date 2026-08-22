-- Single-use links for setting a password without an administrator learning it.
--
-- Migrations are append-only. 00001 through 00015 have run.

-- +goose Up

-- One mechanism serves both cases that need it: a new reader who has an account
-- and no password yet, and an existing reader who has forgotten theirs. Both end
-- with the same act — somebody who is not the administrator choosing a password —
-- so they are one table rather than an invite flow and a reset flow that would
-- drift apart.
--
-- There is no mail in this project and adding it for a household would be a poor
-- trade, so the link is handed over out of band: read out, messaged, typed in. That
-- is also why it expires in days rather than hours.
CREATE TABLE IF NOT EXISTS password_setup_links (
    id          bigserial PRIMARY KEY,

    -- The account this link sets a password for. It always exists first: an
    -- administrator creates the account, then issues a link for it. A link that
    -- created an account on redemption would mean an unauthenticated request
    -- deciding a username, and the delete cascade would have nothing to remove.
    user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- SHA-256 of the token, never the token.
    --
    -- The token is a credential for setting a credential, so the same rule applies
    -- to it as to a password: a copy of this table must not yield anything usable.
    -- SHA-256 rather than argon2id because the input is 256 bits of randomness from
    -- crypto/rand, not something a person chose — there is no dictionary to slow
    -- down, and a redemption should not cost the server a KDF.
    token_sha256 text       NOT NULL UNIQUE,

    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,

    -- Set when redeemed. The row is kept rather than deleted so that presenting a
    -- spent link says "no longer usable" instead of "never existed", and so an
    -- administrator can see that an invitation was taken up.
    used_at     timestamptz
);

-- Redemption looks the row up by hash, which the UNIQUE constraint already indexes.
-- This one serves the other direction: listing or superseding the links belonging to
-- one account.
CREATE INDEX IF NOT EXISTS password_setup_links_user_idx
    ON password_setup_links (user_id);

-- +goose Down

DROP TABLE IF EXISTS password_setup_links;
