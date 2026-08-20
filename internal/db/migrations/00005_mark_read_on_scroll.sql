-- Marking articles read as they are scrolled past, as a preference.
--
-- Migrations are append-only. 00001 through 00004 have run, so this adds.

-- +goose Up

-- The second preference the settings page carries, and still a column rather than
-- a table — 00004 said this could become one when there were several, and two is
-- not several. What would force a table is a preference that is not one value per
-- reader.
--
-- Default false, and that is the load-bearing part: automatic state changes are
-- something a reader opts into. An archive that starts marking things read
-- because it was upgraded would be indistinguishable from a bug, and the reader
-- would have no way to tell which articles it had touched.
ALTER TABLE users ADD COLUMN mark_read_on_scroll boolean NOT NULL DEFAULT false;

-- +goose Down

ALTER TABLE users DROP COLUMN IF EXISTS mark_read_on_scroll;
