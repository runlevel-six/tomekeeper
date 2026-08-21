-- Drops the category column 00013 superseded.
--
-- Migrations are append-only. 00001 through 00013 have run.

-- +goose Up

-- 00013 gave categories their own table and left this column behind on purpose. The
-- reason was internal/db's schema guard, which treats a database newer than the binary
-- as safe on the stated grounds that "the old binary's queries still work against a
-- superset schema" — true only while migrations are additive. Dropping it in 00013
-- would have left a pre-00013 binary passing that guard and then failing on every
-- query naming the column, which is the outage the guard exists to prevent, arriving
-- from the other direction.
--
-- It is safe now because the condition that made it unsafe has passed: every binary
-- anybody would deploy reads category_id, and rolling back far enough to want this
-- column would mean rolling back past a release already in production. The maintainer
-- confirmed that on 2026-08-21, with v0.12.1 deployed.
--
-- The column has been frozen at the 00013 backfill since then — nothing wrote it — so
-- there is nothing here to preserve. A rollback that needs it would have to restore
-- from a dump, which is the honest answer for a column two releases dead rather than
-- pretending otherwise by keeping it.
ALTER TABLE feeds DROP COLUMN IF EXISTS category;

-- +goose Down

-- Recreated empty rather than repopulated, because the values are not recoverable:
-- they were frozen two releases ago and every rename and refile since happened in
-- categories. Backfilled from the current filing on the way down, which is not what
-- the column held but is the only truthful thing available — and is what a binary old
-- enough to read it would want.
ALTER TABLE feeds ADD COLUMN IF NOT EXISTS category text;

UPDATE feeds f SET category = c.name
FROM categories c
WHERE c.id = f.category_id;
