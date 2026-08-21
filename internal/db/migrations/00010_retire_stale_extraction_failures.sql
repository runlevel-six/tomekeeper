-- Retires fetch failures recorded against articles that have since been extracted.
--
-- Migrations are append-only. 00001 through 00009 have run, so this corrects data
-- rather than changing the schema.

-- +goose Up

-- An extraction that produces nothing is recorded by RecordFetchFailure, which writes
-- "extraction produced no content" into fetch_status — a column about fetching. That was
-- survivable while the only cure for such an article was fetching it again, and stopped
-- being survivable once domain rules began rescuing articles from pages already on disk:
-- the body arrives, and nothing ever takes the failure back.
--
-- The attention queue selects on fetch_status, so the effect is that fixing a host does
-- not shrink the queue. Measured on the maintainer's archive on 2026-08-21, immediately
-- after seven new domain rules landed: **409 articles with a good current body still
-- listed as failed, 314 of them extracted by a rule.** A queue that does not empty when
-- you fix things is a queue people stop reading, which is the one thing this archive's
-- maintenance loop cannot afford.
--
-- The code half is store.ClearExtractionFailure, called after every extraction that
-- becomes an article's current body. This is the one-time correction for everything
-- extracted before that existed.
--
-- Both conditions match ClearExtractionFailure exactly, and the reasoning is the same:
--
--   * A stored page (raw_blob_sha) is proof the fetch itself worked, which is the only
--     thing this column is entitled to describe. An imported body whose page fetch
--     genuinely failed keeps its failure and stays in the queue, because the archive
--     really is missing that page.
--   * 'failed' only. A 'skipped' article was refused by robots.txt — a fact about
--     fetching that an extraction cannot settle.
UPDATE articles
SET fetch_status = 'ok',
    fetch_error  = NULL
WHERE fetch_status = 'failed'
  AND raw_blob_sha IS NOT NULL
  AND EXISTS (
        SELECT 1 FROM article_content c
        WHERE c.article_id = articles.id
          AND c.is_current
          AND c.content_html <> ''
  );

-- +goose Down

-- Deliberately empty. The rows this corrected no longer record which of them had been
-- marked failed, and inventing that back would mean re-failing articles that are
-- demonstrably fine. Reverting the code is enough to stop the behavior; nothing needs
-- the old data restored.
SELECT 1;
