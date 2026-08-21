-- Repeats 00010's correction, because a bulk reprocess created fresh instances of it.
--
-- Migrations are append-only. 00001 through 00010 have run.

-- +goose Up

-- 00010 retired the failures recorded against articles that had since been extracted,
-- and store.ClearExtractionFailure keeps that true going forward. Neither covered the
-- *other* direction: an article whose body came from older behavior, whose extraction
-- now produces nothing, was still filed as a failure by the reprocess that found
-- nothing — even though the reader has the article and there is nothing for anybody
-- to fix.
--
-- The version 6 catch-up run did exactly that to eight articles on the maintainer's
-- archive, in a single pass, hours after 00010 cleaned up the last set. The code half
-- is a guard in the extraction job: an ErrNoContent over an article that already has a
-- current body records nothing at all. This is the one-time correction for the rows
-- that run left behind.
--
-- Identical conditions to 00010, for the identical reason: a stored page proves the
-- fetch worked, and an imported body over a genuinely failed fetch keeps its failure
-- because the archive really is missing that page.
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

-- Deliberately empty, as in 00010: which rows had been marked failed is not recorded
-- anywhere after the fact, and re-failing articles that are demonstrably fine would be
-- inventing data. Reverting the code is what stops the behavior.
SELECT 1;
