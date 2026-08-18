-- Retention: letting read articles expire, and marking the ones that must not.
--
-- Migrations are append-only. 00001 and 00002 have run, so this adds rather than
-- edits.

-- +goose Up

-- "Keep this forever", distinct from starring.
--
-- Starring already implies keeping — nobody stars something in order to lose it
-- — but the reverse is not true. A reference page worth holding onto is not
-- necessarily one you liked, and forcing the two through one control would mean
-- a starred list that is really a retention list and useless as a starred list.
ALTER TABLE article_state ADD COLUMN kept boolean NOT NULL DEFAULT false;

-- When this article's stored body and images were deleted.
--
-- On the shared article row rather than per-user, because the thing it describes
-- is shared: article_content and assets are a global pool (§2.8), so expiry is a
-- fact about the archive, not about one reader. This does not violate the rule
-- against user-specific data on shared rows — no user's decision is recorded
-- here, only the outcome after every user's decision was consulted.
--
-- The row itself survives. Title, URL and date remain, so search still knows the
-- article existed, deduplication still works, and read state is not lost. Only
-- the expensive part goes.
ALTER TABLE articles ADD COLUMN content_expired_at timestamptz;

-- The expiry scan walks article_state looking for finished readers. Partial,
-- because the rows that can never expire — starred, kept, saved, unread — are
-- exactly the ones worth not indexing, and in a well-curated archive they are a
-- large fraction of the table.
CREATE INDEX article_state_expirable_idx
  ON article_state (article_id, read_at)
  WHERE read AND NOT starred AND NOT kept AND saved_at IS NULL;

-- Finding candidates starts from articles that still have a body to lose.
CREATE INDEX articles_unexpired_idx
  ON articles (id)
  WHERE content_expired_at IS NULL;

-- +goose Down

DROP INDEX IF EXISTS articles_unexpired_idx;
DROP INDEX IF EXISTS article_state_expirable_idx;
ALTER TABLE articles DROP COLUMN IF EXISTS content_expired_at;
ALTER TABLE article_state DROP COLUMN IF EXISTS kept;
