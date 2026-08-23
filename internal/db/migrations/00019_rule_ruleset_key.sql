-- Stores each rule's ruleset key, so staleness is a comparison the database can make.
--
-- Migrations are append-only. 00001 through 00018 have run.

-- +goose Up

-- The same key that 00018 put on each body, now on the rule that would produce one.
--
-- A body is stale for a reader when the key it carries differs from the key of the
-- rule that applies to it. With the key on both sides that is a join and an
-- inequality; without it the sweep would have to fetch every rule, compute a hash
-- in Go, and filter in memory — or compute the hash in SQL, which would mean two
-- implementations of one hash that have to agree byte for byte forever.
--
-- Written by the store on every upsert rather than generated, because the hash is
-- defined in Go: EffectiveRule.RulesetKey is the single definition, and a column
-- Postgres computed would be a second one.
--
-- Empty is right for existing rows until they are next written. It makes every
-- body on those hosts look stale exactly once, which costs one re-extraction that
-- lands on the same result — the same trade 00018 already took.
ALTER TABLE domain_rules ADD COLUMN IF NOT EXISTS ruleset_key text NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE domain_rules DROP COLUMN IF EXISTS ruleset_key;
