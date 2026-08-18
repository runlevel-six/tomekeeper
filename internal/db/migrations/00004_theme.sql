-- The reader's chosen palette.
--
-- Migrations are append-only. 00001 through 00003 have run, so this adds.

-- +goose Up

-- On users rather than in a cookie, so the choice follows the reader between
-- their phone and their desk rather than being a property of one browser. It is
-- one column because that is genuinely all the state there is; when the settings
-- page grows a second and third preference this can become a table without any
-- of them having needed one first.
--
-- 'auto' means the original neutral palette following the system's light/dark
-- preference, which is what every existing reader already has and must keep
-- having after this migration runs.
ALTER TABLE users ADD COLUMN theme text NOT NULL DEFAULT 'auto';

-- +goose Down

ALTER TABLE users DROP COLUMN IF EXISTS theme;
