-- Fetching article pages and extracting their bodies.
--
-- Migrations are append-only. 00001 has run, so corrections go here rather
-- than into that file.

-- +goose Up

-- The feed's own body, kept for the last rung of the extraction ladder.
--
-- The poller design requires the feed body as a fallback when fetching the page fails, and
-- The initial schema stored only the summary. The two are different:
-- feed_summary is the short
-- teaser most feeds carry, while this is content:encoded, which is sometimes
-- the entire article and is the only surviving copy when a site goes down
-- between publication and the next poll.
ALTER TABLE feed_items ADD COLUMN feed_content text;

-- Where the raw fetch was stored in the blob tree.
--
-- The path could almost be derived from the article's date and title, but not
-- safely: a title can change on a later poll, and a derived path would then
-- point at nothing. Storing it explicitly means the raw fetch remains findable
-- however the metadata drifts, which is what `tome reextract` depends on.
ALTER TABLE articles ADD COLUMN raw_blob_path text;

-- The extract worker's hot path: articles fetched but not yet extracted.
CREATE INDEX articles_awaiting_extraction_idx ON articles (first_seen_at)
  WHERE fetch_status = 'ok' AND raw_blob_sha IS NOT NULL;

-- `tome reextract --since-version` scans current, mutable content rows by
-- version. Without this it is a sequential scan of the whole archive.
CREATE INDEX article_content_version_idx
  ON article_content (extractor_version)
  WHERE is_current AND NOT immutable;

-- +goose Down
DROP INDEX IF EXISTS article_content_version_idx;
DROP INDEX IF EXISTS articles_awaiting_extraction_idx;
ALTER TABLE articles DROP COLUMN IF EXISTS raw_blob_path;
ALTER TABLE feed_items DROP COLUMN IF EXISTS feed_content;
