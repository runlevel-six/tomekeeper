-- Lets a reader's association with an article lapse without the article going
-- with it.
--
-- Migrations are append-only. 00001 through 00019 have run.

-- +goose Up

-- A reader's retention window says how long they hold on to what they have read.
-- When it lapses their association is forgotten — and the article stays until every
-- reader has reached that point, because it is the household's.
--
-- The row survives as a tombstone rather than being deleted, and that is not a
-- detail: ExpirableArticles reads "no state row" as **never seen it**, in two
-- separate clauses. Deleting the rows would make an article that everybody has
-- forgotten look like one nobody has opened — permanently unexpirable, and counted
-- as unread against a subscriber who is done with it. The exact opposite of the
-- intent, and silent.
--
-- What the tombstone keeps is the fact that this reader is finished. What it drops
-- is everything the reader might not want kept: when they read it, whether they
-- starred or saved it, and their highlights. That costs no privacy they had, for
-- the case it applies to: an article reachable from a feed they subscribe to is
-- already associated with them through feed_items, so the row reveals nothing new.
-- An article reachable *only* through their state — something saved by hand — has
-- its row deleted outright instead, leaving the article referenced by nobody, which
-- is `tome prune`'s case.
ALTER TABLE article_state ADD COLUMN IF NOT EXISTS forgotten_at timestamptz;

-- The forgetting sweep asks one question per reader: which of my read articles are
-- past my window and not yet forgotten. Partial, because a tombstone is never a
-- candidate again and the index has no reason to carry one.
CREATE INDEX IF NOT EXISTS article_state_forgettable_idx
    ON article_state (user_id, read_at)
    WHERE read AND forgotten_at IS NULL AND NOT starred AND NOT kept AND saved_at IS NULL;

-- Expiry asks the opposite question — is anybody still holding on — so it needs to
-- find claims rather than candidates.
CREATE INDEX IF NOT EXISTS article_state_claims_idx
    ON article_state (article_id) WHERE forgotten_at IS NULL;

-- +goose Down

DROP INDEX IF EXISTS article_state_claims_idx;
DROP INDEX IF EXISTS article_state_forgettable_idx;
ALTER TABLE article_state DROP COLUMN IF EXISTS forgotten_at;
