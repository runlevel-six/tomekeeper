-- Categories become things rather than strings.
--
-- Migrations are append-only. 00001 through 00012 have run, so this adds.

-- +goose Up

-- A category has been free text on a subscription since the beginning: it exists
-- exactly as long as some feed claims one, which makes "create a category" an
-- operation with no object and "delete a category" a rewrite of the feeds filed
-- under it. That is survivable for a filter and not for a thing a reader manages.
--
-- The decision to give them identity was not made for the interface, though. It was
-- made because **the Fever group id is a hash of the category's name**: the protocol
-- requires an id, there was no row to take one from, so `feverGroupIDs` derives one
-- from the name with collision rehashing. Clients cache folder membership against
-- those ids — so renaming a category silently reshuffled somebody's reader, the old
-- folder vanishing and a new one appearing with the same contents. An id that
-- belongs to the category rather than to its current name is the fix, and it retires
-- the hash.
CREATE TABLE categories (
  id      bigserial PRIMARY KEY,
  user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name    text   NOT NULL,

  -- Scoped per reader, like feeds. Two readers may each have a "Tech" and they are
  -- not the same category; the unique constraint says so.
  --
  -- Case-sensitive deliberately: renaming "tech" to "Tech" has to be possible, and a
  -- case-insensitive constraint would refuse it as a duplicate of itself.
  UNIQUE (user_id, name),

  -- A name has to be something. An empty one would be a second way to say
  -- "no category", and there is already a way to say that: no row at all.
  CONSTRAINT categories_name_not_blank CHECK (btrim(name) <> '')
);

COMMENT ON TABLE categories IS
  'A reader''s folders. Nullable from feeds: the absence of a category is expressed by feeds.category_id IS NULL, never by a row, because "no folder" is not a folder that could be renamed or deleted.';

ALTER TABLE feeds ADD COLUMN category_id bigint REFERENCES categories(id) ON DELETE SET NULL;

-- ON DELETE SET NULL rather than RESTRICT or CASCADE, and this is the whole of the
-- delete-a-category decision expressed in the schema:
--
--   * CASCADE would delete the feeds, and deleting a subscription is a separate
--     deliberate act that already exists and already asks first.
--   * RESTRICT would make deleting a non-empty category impossible, which is exactly
--     the case a reader wants it for.
--
-- SET NULL leaves the feeds filed nowhere, which is a state the interface already
-- draws. **No article is touched by any of this**: nothing in this project deletes an
-- article, and an article has no category of its own to change — it is derived
-- through feed_items to the feed that carried it. Refiling a feed moves everything it
-- ever brought in, retroactively, because there is nothing else it could mean.
CREATE INDEX feeds_category_idx ON feeds (category_id) WHERE category_id IS NOT NULL;

-- The backfill. One row per distinct non-empty name per reader, taken from the column
-- that has been carrying them.
--
-- Note what is *not* backfilled: a feed whose category is NULL or empty gets no row
-- and keeps category_id NULL. A reader with a category literally named
-- "Uncategorized" — which is what a FreshRSS export produces for feeds that had none,
-- and the OPML importer takes folder names verbatim — keeps it as an ordinary
-- category. It is now distinguishable from having no category at all, which it was
-- not before, and refiling those feeds to no category is how it gets merged. That is
-- a reader's decision and not a migration's.
INSERT INTO categories (user_id, name)
SELECT DISTINCT user_id, btrim(category)
FROM feeds
WHERE btrim(COALESCE(category, '')) <> ''
ON CONFLICT (user_id, name) DO NOTHING;

UPDATE feeds f
SET category_id = c.id
FROM categories c
WHERE c.user_id = f.user_id
  AND c.name = btrim(f.category);

-- feeds.category stays, and is deliberately not dropped here.
--
-- internal/db's schema guard treats a database newer than the binary as fine, on the
-- stated grounds that "the old binary's queries still work against a superset
-- schema". That is only true while migrations are additive. Dropping this column
-- would leave an older binary passing the guard and then failing on every query that
-- names it — which is the outage that guard was written to prevent, reintroduced from
-- the other direction.
--
-- So: the new code reads and writes category_id only, this column is frozen at the
-- values above, and a later migration drops it once no deployable binary reads it. A
-- rollback in the meantime sees categories as they were at this migration, which is
-- degraded rather than broken.
COMMENT ON COLUMN feeds.category IS
  'Superseded by category_id. Frozen at the 00013 backfill and kept only so that a rollback to a pre-00013 binary still works; nothing writes it. Drop once no deployable binary reads it.';

-- +goose Down

DROP INDEX IF EXISTS feeds_category_idx;
ALTER TABLE feeds DROP COLUMN IF EXISTS category_id;
DROP TABLE IF EXISTS categories;
