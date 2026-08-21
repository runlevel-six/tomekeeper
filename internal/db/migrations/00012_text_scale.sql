-- How large the reader wants the type.
--
-- Migrations are append-only. 00001 through 00011 have run, so this adds.

-- +goose Up

-- On users rather than in a cookie, for the reason the palette is: the choice
-- follows the reader between their phone and their desk. But there is a second
-- reason here that the palette only hinted at — a size preference has to be in
-- the *first paint*. A palette read by script flashes the wrong colors; a size
-- read by script reflows the entire page after it has already been laid out and
-- read, which is worse than either the old size or the new one.
--
-- A named step rather than a number or a percentage. Three reasons, in order of
-- how much they matter:
--
--   * The content security policy has no 'unsafe-inline' for styles, so the value
--     cannot travel as a style attribute on <html>. It travels as a data
--     attribute the stylesheet maps, exactly like data-theme — and a stylesheet
--     can only map values it knows the names of.
--   * A free number invites 1.03, which is indistinguishable from 1.0 and makes
--     the setting look broken.
--   * A name survives a change of mind about what it should measure. The steps
--     below are ratios of the root font size today; if that turns out to be the
--     wrong mechanism, 'larger' still means larger.
--
-- 'normal' is what every existing reader has and must keep having.
ALTER TABLE users ADD COLUMN text_scale text NOT NULL DEFAULT 'normal';

COMMENT ON COLUMN users.text_scale IS
  'Named type-size step: smaller, normal, larger, largest. Rendered into the first paint as a data attribute the stylesheet maps to a root font-size ratio; never read by script.';

-- +goose Down

ALTER TABLE users DROP COLUMN IF EXISTS text_scale;
