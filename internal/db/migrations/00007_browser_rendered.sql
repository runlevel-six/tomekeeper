-- Records that an article's stored page came out of a browser rather than off the wire.
--
-- Migrations are append-only. 00001 through 00006 have run, so this adds.

-- +goose Up

-- A column rather than an inference, and the distinction matters. The obvious
-- alternative is to look up the domain rule when the question is asked — but rules
-- change, so an article fetched plainly in March would start claiming it had been
-- rendered the moment somebody flagged its domain in September. A stored page is a
-- historical fact about one fetch and has to be recorded at the time.
--
-- NOT NULL DEFAULT false, unlike 00006's deliberately-nullable columns, because here
-- there is no third state: every page already in the archive was fetched directly,
-- which is exactly what false says about it. Nothing has to be invented for the
-- 2,087 rows that exist.
--
-- Written by the render job and read by `tome explain`, the attention queue and the
-- article page. That last part is not decoration: an article whose body looks odd is a
-- different investigation depending on whether a browser was involved, and the one
-- thing worse than not recording it is recording it and never reading it — see
-- article_content.fs_path, a column nothing has ever written.
ALTER TABLE articles
  ADD COLUMN browser_rendered boolean NOT NULL DEFAULT false;

-- No index. The column is read one article at a time, and the only aggregate anybody
-- wants — "how many pages did we render" — is a rare question over a table small
-- enough to scan.

-- +goose Down

ALTER TABLE articles DROP COLUMN browser_rendered;
