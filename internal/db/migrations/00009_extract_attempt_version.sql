-- Records which extractor version last tried an article, so that a *failed* extraction
-- can be reprocessed like a successful one.
--
-- Migrations are append-only. 00001 through 00008 have run, so this adds.

-- +goose Up

-- This column exists because `tome reextract` had a blind spot over exactly the articles
-- it was written for.
--
-- Reprocessing selects articles whose current body came from an older extractor — which
-- requires a current body. An article whose extraction produced nothing has none, so it
-- was never a candidate, and every extraction improvement since the second milestone
-- silently skipped it. Measured on the maintainer's archive on 2026-08-21: **343 articles
-- with a stored page and no body**, of which 280 are webcomics from one host — and the
-- rung written specifically for image-only pages, three versions ago, would archive them
-- today. Their pages have been on disk since the first poll with nothing able to point at
-- them.
--
-- A failure needs somewhere to record the version that failed, so that the same "other
-- than this version" comparison works for both outcomes. That is what this is: the
-- version of the last extraction *attempt*, whatever came of it.
--
-- Nullable, and NULL is the value every existing row has: "never attempted, or attempted
-- before this column existed". It has to compare as *different* from any version, which
-- is why the query uses IS DISTINCT FROM rather than <> — `NULL <> '5'` is NULL, not
-- true, and a plain inequality would have silently excluded every article this column was
-- added to reach. That is the same class of mistake as the one above, and it would have
-- looked identical from outside.
ALTER TABLE articles
  ADD COLUMN extract_attempt_version text;

COMMENT ON COLUMN articles.extract_attempt_version IS
  'Extractor version of the last extraction attempt, successful or not. NULL means never attempted since this column existed. Compared with IS DISTINCT FROM when selecting articles to reprocess, so that NULL counts as out of date.';

-- +goose Down

ALTER TABLE articles DROP COLUMN extract_attempt_version;
