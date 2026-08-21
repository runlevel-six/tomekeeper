-- Records how much visible text the stored page actually had.
--
-- Migrations are append-only. 00001 through 00007 have run, so this adds.

-- +goose Up

-- This is the number that tells a shell from a badly-structured page, and until now it
-- was computed during extraction and thrown away — so the only way to ask it was
-- `tome explain` on a distroless pod through `kubectl exec`. That made "does this site
-- need a browser or a CSS selector?" a question a reader could not answer in the
-- interface, which is the wrong place for the answer to live: the failed-fetch queue is
-- where a site that needs attention is *found*.
--
-- Nullable, and NULL is meaningful: it means nobody has extracted this article since the
-- column existed. Every row in the archive starts that way and fills in on the next
-- re-extraction, so the interface has to say "not measured" rather than "zero" — a
-- default of 0 would have claimed every existing article served an empty page, which is
-- exactly the sort of confident wrong answer this column exists to prevent.
--
-- On articles rather than article_content because it describes the *page* that was
-- stored, not the body that was derived from it. Two bodies extracted from one page at
-- different extractor versions measured the same page.
ALTER TABLE articles
  ADD COLUMN page_visible_chars int;

COMMENT ON COLUMN articles.page_visible_chars IS
  'Visible text length of the stored page, measured at extraction. NULL means not measured since this column existed. A few hundred characters means the page is a shell whose content arrives by JavaScript.';

-- +goose Down

ALTER TABLE articles DROP COLUMN page_visible_chars;
