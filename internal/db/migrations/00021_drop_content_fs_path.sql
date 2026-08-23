-- Removes a column that has never held a value.
--
-- Migrations are append-only. 00001 through 00020 have run.

-- +goose Up

-- `article_content.fs_path` was meant to record where a body's standalone page was
-- written in the archive tree. Nothing ever wrote it: `ContentParams` carried the
-- field, neither extraction nor import set it, and on the live archive it was NULL
-- on all 10,161 rows. Nothing read it either — the path is derived from the article
-- identically at each call site, which is why its absence was never noticed.
--
-- It goes rather than being populated, and the reason is what 1.0 means: the schema
-- is part of the interface this release promises to keep. A documented column that
-- always lies by omission is the same shape as `assets_status = 'pending'` being a
-- terminal state wearing a transient label, and that one cost real articles before
-- anybody noticed. A promise nothing keeps is worse than no promise.
--
-- `assets.fs_path` is a different column on a different table and stays: it is
-- written for every stored image and read by the export, the retention sweep and
-- the article page.
--
-- Nothing is lost. There is no data in it to migrate, and the down migration
-- restores the column exactly as it was — NULL everywhere, which is what it held.
ALTER TABLE article_content DROP COLUMN fs_path;

-- +goose Down

ALTER TABLE article_content ADD COLUMN fs_path text;
