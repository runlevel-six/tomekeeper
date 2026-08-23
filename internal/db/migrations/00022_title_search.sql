-- Makes titles searchable.
--
-- Migrations are append-only. 00001 through 00021 have run.

-- +goose Up

-- Search matched `article_content.tsv` and nothing else, so an article whose
-- distinctive words appear only in its **title** could not be found. Discovered
-- while running the multi-user acceptance drill on 2026-08-23: searching "Desktop"
-- for an article titled "An Atari Desktop On A Sega" returned nothing, which looked
-- for a moment like a scoping failure and was a gap in what is indexed.
--
-- It is a real gap rather than a curiosity. A title is what a reader remembers — it
-- is the string they saw in the list — and a body legitimately need not repeat it.
-- The article in question was a video post whose prose never used the word.
--
-- **Its own column on `articles` rather than folding the title into
-- `article_content.tsv`.** A generated column may only read the row it belongs to,
-- and the title lives on a different table from the body. That separation is also
-- correct beyond the constraint: a title belongs to the household — one row on
-- `articles`, shared by every reader — while a body may be forked per reader, so one
-- combined vector would have to be maintained once per fork to carry a string that
-- is identical in all of them.
--
-- `english`, matching the body's column. The reasoning is unchanged and written on
-- `searchConfig`: a per-row configuration cannot be used in a generated column,
-- because the cast needed to read one is only stable and Postgres requires immutable.
ALTER TABLE articles
    ADD COLUMN title_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('english', coalesce(title, ''))) STORED;

-- GIN, like the body's. Titles are short, so this index is a fraction of the size of
-- the one over the bodies: about 2,300 titles against 2,300 articles' prose.
CREATE INDEX IF NOT EXISTS articles_title_tsv_idx ON articles USING gin (title_tsv);

-- +goose Down

DROP INDEX IF EXISTS articles_title_tsv_idx;
ALTER TABLE articles DROP COLUMN title_tsv;
