-- A reader's own polling cadence: one general preference, and a per-feed override.
--
-- Migrations are append-only. 00001 through 00005 have run, so this adds.

-- +goose Up

-- Both columns are nullable, and NULL is the load-bearing value: it means "decide
-- for me", which is what every existing row has and what the adaptive interval has
-- always done. A NOT NULL DEFAULT would have to invent a cadence for 74 feeds
-- nobody chose one for, and would make "automatic" unreachable afterwards.
--
-- Separate from feeds.poll_interval rather than writing over it. That column is
-- where the poller keeps what it has learned, and it is rewritten on every poll; a
-- reader's choice has to survive being polled. Keeping them apart is also what lets
-- an override be removed and leave the learned interval to carry on from.
ALTER TABLE feeds ADD COLUMN poll_interval_override interval;

-- The general preference, on the reader rather than on each feed, so that setting
-- it once covers a subscription list that grows afterwards. Third preference on
-- users and still a column, for the reason 00005 gave: what would force a
-- preferences table is a preference that is not one value per reader.
ALTER TABLE users ADD COLUMN default_poll_interval interval;

-- No CHECK bounding either column. The politeness floor is
-- TOME_POLL_MIN_INTERVAL, which an operator can change between two runs of one
-- binary — a constraint written against today's value would refuse a row that
-- yesterday's configuration allowed, and the clamp has to happen at poll time
-- anyway. The interval type's own range is the only limit here.

-- +goose Down

ALTER TABLE users DROP COLUMN IF EXISTS default_poll_interval;
ALTER TABLE feeds DROP COLUMN IF EXISTS poll_interval_override;
