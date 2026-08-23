-- Records what produced a body, and makes a host something the database can index.
--
-- Migrations are append-only. 00001 through 00017 have run.

-- +goose Up

-- The host, computed once and stored, rather than by split_part in every query
-- that needs it.
--
-- Three places already wanted this and each recomputed it per row over an
-- unindexed scan: re-extraction candidate selection, `reextract --domain`, and the
-- domain-rule lookup. It matters more now, because a rule change has to find the
-- articles it affects — the largest host in a real archive here is 289 articles out
-- of 2,300, so the difference is an index lookup against a full scan.
--
-- GENERATED ... STORED rather than a trigger: split_part is immutable, so Postgres
-- maintains it, it cannot drift from url_canonical, and no code can forget to set
-- it. It parses scheme://host[:port]/path, which is what the existing expression
-- did, so nothing changes about which host a URL yields.
ALTER TABLE articles ADD COLUMN IF NOT EXISTS host text
    GENERATED ALWAYS AS (
        split_part(split_part(split_part(url_canonical, '://', 2), '/', 1), ':', 1)
    ) STORED;

CREATE INDEX IF NOT EXISTS articles_host_idx ON articles (host);

-- What produced this body, beyond the extractor version already recorded beside it.
--
-- A body is stale when the program that made it has moved on — which
-- extractor_version already says — **or** when the rules that shaped it have. The
-- second half had nowhere to live, so nothing could tell a body extracted under a
-- reader's selector from one extracted without it, and "does this need
-- re-extracting for this reader" was not a question the database could answer.
--
-- That question needs answering for a reason this deployment makes ordinary rather
-- than exceptional: the server and the worker are separate Deployments, so a reader
-- can change a rule while the worker is down — during a rollout, a migration, or an
-- OOM. Work enqueued eagerly at that moment is simply lost. Every other stage in
-- this pipeline already pairs eager enqueueing with a sweep that re-derives what is
-- outstanding, for exactly this case; this column is what lets extraction have one
-- too.
--
-- Empty means "no rule applied", which is a real value and distinct from a rule
-- that happens to select nothing. Existing rows get the default and are therefore
-- all treated as having been extracted without a rule — untrue for the 219 bodies a
-- domain rule produced, and harmless: it makes them candidates for one
-- re-extraction that lands on the same result.
ALTER TABLE article_content ADD COLUMN IF NOT EXISTS ruleset_key text NOT NULL DEFAULT '';

-- Only the extraction half of a rule may belong to a reader.
--
-- content_selector and strip_selectors decide how a stored page becomes a body, so
-- two readers can hold different ones and each get their own extraction. The rest —
-- requires_js, user_agent, rate_limit_rps — decide how the page is *fetched*, and
-- the household owns the fetch: there is one raw copy of a page and one request to
-- the origin that produced it. A reader cannot ask for a browser render of a page
-- that has already been fetched without one, because there is nothing per-reader
-- for that setting to act on.
--
-- Enforced here rather than trusted to the application, because a rule row that
-- quietly carries a fetch setting nobody honors is a setting that looks configured
-- and does nothing.
ALTER TABLE domain_rules DROP CONSTRAINT IF EXISTS domain_rules_reader_extraction_only;
ALTER TABLE domain_rules ADD CONSTRAINT domain_rules_reader_extraction_only CHECK (
    user_id IS NULL
    OR (NOT requires_js AND user_agent IS NULL AND rate_limit_rps IS NULL)
);

-- +goose Down

ALTER TABLE domain_rules DROP CONSTRAINT IF EXISTS domain_rules_reader_extraction_only;
ALTER TABLE article_content DROP COLUMN IF EXISTS ruleset_key;
DROP INDEX IF EXISTS articles_host_idx;
ALTER TABLE articles DROP COLUMN IF EXISTS host;
