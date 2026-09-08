-- Records the picture at the top of each body, so a list can show a thumbnail.
--
-- Migrations are append-only. 00001 through 00022 have run.

-- +goose Up

-- A list of headlines is what this archive shows on every reading page, and the
-- thing a reader recognizes an article by is often the picture that came with it.
-- Every image is already stored and content-addressed; what was missing was a way
-- to name *which* of an article's images is the one to show, cheaply enough to ask
-- fifty times per page.
--
-- **Derived once and stored, rather than read out of the body at query time.**
-- The body already holds the answer — the first `src="/assets/…"` in the HTML is
-- the first picture the reader sees — and a regular expression over `content_html`
-- needs no new column at all. Measured against the live archive, on the actual
-- stream query for the unread list: 114ms without it, 460ms with. That is four
-- times the cost of the whole page's query, paid on every scroll fragment, to
-- re-derive something that changes only when the body does. `content_html` is
-- TOASTed out of line, so the regular expression drags every body of the page off
-- disk to read its first hundred bytes.
--
-- **On `article_content` rather than on `articles`**, because a body is what an
-- image belongs to and bodies are per-reader: a reader whose own extraction picks
-- a different region of the page has a different first picture, and a superseded
-- body keeps the one it had, so promoting an older copy brings its thumbnail back
-- with it. An article-level column would have to pick one reader's answer for
-- everybody, which is exactly the sharing the tenancy line forbids.
--
-- **A content address with a foreign key, not a path.** The path is one join away
-- (`assets.fs_path`), and the reference is what makes a broken thumbnail
-- unrepresentable rather than merely unlikely. Releasing an article today deletes
-- its bodies before the images they name — `ExpireArticle` does both, and `tome
-- prune` calls it — so nothing is left pointing anywhere, and this column would be
-- correct with no constraint at all. The constraint is for the release path that
-- does not exist yet: ON DELETE SET NULL means a stored image can never be removed
-- while a row still claims to show it. A copied path would have to be swept by
-- hand, and the column nothing sweeps is the column that lies.
ALTER TABLE article_content
    ADD COLUMN lead_image_sha text REFERENCES assets(sha256) ON DELETE SET NULL;

-- The referencing side of that foreign key. Without it every asset deletion scans
-- article_content to prove nothing points at the row, and retention deletes assets
-- an article at a time.
CREATE INDEX article_content_lead_image_idx
    ON article_content (lead_image_sha) WHERE lead_image_sha IS NOT NULL;

-- The backfill, so the column is true for the archive that already exists rather
-- than only for what is localized from here on. Localization writes it going
-- forward; this is the same derivation, run once.
--
-- Every localized image reference in a stored body is `/` followed by the asset's
-- `fs_path` — that is what `asset.LocalPrefix` means — so the join is exact rather
-- than a guess. Run against the live archive before being written here: 2,255 of
-- 3,654 current bodies contain such a reference and all 2,255 resolved to a row in
-- `assets`. Bodies with no localized image match nothing and stay NULL, which is
-- the same thing they will say after the localizer next runs over them.
--
-- Superseded bodies are backfilled too, not just the current one: promoting an
-- older copy makes it current, and a thumbnail that vanished on promotion would be
-- a puzzle with no explanation on the page.
UPDATE article_content c
   SET lead_image_sha = a.sha256
  FROM assets a
 WHERE '/' || a.fs_path = substring(c.content_html from 'src="(/assets/[^"]+)"');

-- +goose Down

DROP INDEX IF EXISTS article_content_lead_image_idx;
ALTER TABLE article_content DROP COLUMN lead_image_sha;
