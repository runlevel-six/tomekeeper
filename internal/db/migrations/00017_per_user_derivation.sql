-- Gives bodies, domain rules and retention an owner, so two readers can extract
-- one shared page differently.
--
-- Migrations are append-only. 00001 through 00016 have run.

-- +goose Up

-- The line this migration draws:
--
--   The household owns what costs bandwidth or disk — the fetched page and the
--   content-addressed assets. One poll, one raw copy, one image however many
--   readers hold it. That is 63% of this archive and no comparable tool manages
--   it.
--
--   The reader owns everything derived from that — the body, the rules that
--   produced it, and which stored copy they see.
--
-- Every current body in this archive is about 10% of its bytes, so a reader who
-- forked every single one would cost a tenth. Miniflux re-polls the same feed per
-- subscriber; Wallabag stores the extracted content per user; Karakeep duplicates
-- the images too. Duplicating bodies is the cheap end of what the field does
-- anyway.

-- NULL means the household default: the extraction everybody gets unless they
-- have asked for something else. Every row that exists today becomes one, which
-- is why this migration rewrites no data.
--
-- Copy-on-write. A reader gets a row of their own only when their extraction
-- actually diverges — which, for readers who never write a domain rule, is never.
-- Two readers with identical custom rules do get two rows; that is a bounded cost
-- nobody pays until they ask for it, and the alternative is keying bodies by a
-- hash of the effective ruleset, which buys deduplication in a case a household
-- will not hit.
ALTER TABLE article_content ADD COLUMN IF NOT EXISTS user_id bigint
    REFERENCES users(id) ON DELETE CASCADE;

-- One current body per article *per reader*, with the household default as its own
-- slot.
--
-- COALESCE rather than a partial index per case, because NULL is not distinct from
-- NULL in a unique index — without it, an article could accumulate any number of
-- current household bodies, which is exactly the invariant the old index existed
-- to hold.
DROP INDEX IF EXISTS article_content_current_idx;
CREATE UNIQUE INDEX IF NOT EXISTS article_content_current_idx
    ON article_content (article_id, COALESCE(user_id, 0)) WHERE is_current;

-- Reading a body means "mine, or the household's" — one index serving both halves
-- of that lookup.
CREATE INDEX IF NOT EXISTS article_content_owner_idx
    ON article_content (article_id, user_id) WHERE is_current;

-- Domain rules get the same treatment, and for the stronger reason: a rule is what
-- makes two readers' extractions differ in the first place.
--
-- NULL is again the household default. A default anybody may override is not the
-- same thing as a decision made on somebody's behalf — and the alternative, every
-- reader writing their own selector for the same broken comic site, is worse for
-- everyone. What is deliberately accepted here: changing a household rule changes
-- what every reader who has not overridden it sees, in the same way that improving
-- the extractor does.
ALTER TABLE domain_rules ADD COLUMN IF NOT EXISTS user_id bigint
    REFERENCES users(id) ON DELETE CASCADE;

-- The old uniqueness was one rule per domain. It is now one rule per domain per
-- reader, with the household default in its own slot.
ALTER TABLE domain_rules DROP CONSTRAINT IF EXISTS domain_rules_domain_key;
DROP INDEX IF EXISTS domain_rules_domain_key;
CREATE UNIQUE INDEX IF NOT EXISTS domain_rules_owner_domain_idx
    ON domain_rules (domain, COALESCE(user_id, 0));

-- Retention becomes a reader's setting, and it is worth being honest in the schema
-- about what it can and cannot do.
--
-- It expires *that reader's view*: their state, and their own forked body if they
-- have one. It cannot reclaim the shared page or the shared images, because
-- another reader may still be holding them — that is `tome prune`'s job, and it
-- operates on what nothing references at all.
--
-- NULL means "follow the household default" (TOME_RETAIN_AFTER_READ), which is
-- also what every existing row gets. Zero is a real value meaning "keep
-- everything" and is deliberately distinct from NULL.
ALTER TABLE users ADD COLUMN IF NOT EXISTS retain_after_read interval;

-- +goose Down

ALTER TABLE users DROP COLUMN IF EXISTS retain_after_read;

DROP INDEX IF EXISTS domain_rules_owner_domain_idx;
ALTER TABLE domain_rules DROP COLUMN IF EXISTS user_id;
-- Restoring the old uniqueness can fail, and should: if two readers wrote rules
-- for the same host, there is no one rule to roll back to, and silently keeping
-- one of them would be choosing whose extraction survives.
ALTER TABLE domain_rules ADD CONSTRAINT domain_rules_domain_key UNIQUE (domain);

DROP INDEX IF EXISTS article_content_owner_idx;
DROP INDEX IF EXISTS article_content_current_idx;
-- Same shape: a rollback with forked bodies present would have to pick one per
-- article. Deleting the forks first is the caller's decision to make deliberately.
DELETE FROM article_content WHERE user_id IS NOT NULL;
ALTER TABLE article_content DROP COLUMN IF EXISTS user_id;
CREATE UNIQUE INDEX article_content_current_idx
    ON article_content (article_id) WHERE is_current;
