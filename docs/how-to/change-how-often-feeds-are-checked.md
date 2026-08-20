# How to change how often feeds are checked

By default nothing here is configured: each feed's interval is learned from its own
behavior, between `TOME_POLL_MIN_INTERVAL` and `TOME_POLL_MAX_INTERVAL` (15 minutes
and 24 hours out of the box). That is the right answer for most subscriptions and the
wrong one for the few whose timing you care about — a feed that publishes twice a
year has been backed off to daily by the time it publishes.

There are two settings for that, and they interact in one direction only: the
per-feed one wins.

## All your feeds

**Settings** → **Checking feeds** → *Check my feeds*.

Choosing an interval holds every feed to it. Choosing **Automatically** — the default
— hands each feed back to the learned interval.

Shortening the cadence brings feeds forward, but only to `last poll + the new
interval`, so a 70-feed list settles into the new rhythm rather than arriving at the
worker at once. Feeds with a cadence of their own are left alone, as are disabled
ones. Lengthening it never postpones a poll that was already imminent.

## One feed

**Feeds** → **Edit** on its row → *Check it*.

This is an override, so the picker always opens on **Automatically** for a feed you
have not set one on — even when you have a general cadence. Choosing an interval here
holds this feed to it whatever Settings says; choosing **Automatically** again hands
it back to your general cadence, or to the learned interval if you have none.

The confirmation line after saving says which of the three is now in force. There is
no column for it in the feed list, so that line is where the change is visible.

## What a choice cannot do

- **Poll more often than `TOME_POLL_MIN_INTERVAL`.** That floor is a promise made to
  other people's servers, so a shorter choice is raised to it. The picker leaves out
  anything below the floor for that reason — if you want 5-minute checks, that is an
  operator decision, made with `TOME_POLL_MIN_INTERVAL` on the worker.
- **Keep a broken feed on schedule.** Failures back off exponentially and a feed is
  disabled after `TOME_FEED_FAILURE_THRESHOLD` consecutive ones. Your cadence is a
  floor on that backoff, never a ceiling: a feed set to weekly is not polled every 15
  minutes because it started failing, but a feed set to hourly that has failed six
  times running is not polled hourly either. See
  [Troubleshoot a failing feed](troubleshoot-a-failing-feed.md).
- **Fetch anything right now.** A cadence changes the schedule; **Check all feeds
  now** on the **Feeds** page is the button that makes everything due. Both leave the
  fetching to the worker, which picks up due feeds within a minute.

Going the other way is unrestricted: `TOME_POLL_MAX_INTERVAL` bounds only what this
service does on its own initiative, so **Once a week** is honored as given.

## From SQL

The columns are `users.default_poll_interval` and `feeds.poll_interval_override`, and
either being `NULL` means "not set" rather than "zero":

```sql
-- Every feed, hourly.
UPDATE users SET default_poll_interval = interval '1 hour' WHERE id = $1;

-- Except this one, which is worth checking every 15 minutes.
UPDATE feeds SET poll_interval_override = interval '15 minutes' WHERE id = $2;

-- Back to learned intervals everywhere.
UPDATE users SET default_poll_interval = NULL WHERE id = $1;
UPDATE feeds SET poll_interval_override = NULL WHERE user_id = $1;
```

Do not write to `feeds.poll_interval`: that is where the poller keeps what it has
learned, and it is rewritten on the next poll. Setting either column by hand does not
move `next_poll_at`, so a shortened cadence takes effect at the next scheduled poll
rather than sooner — the interface's own controls are what bring feeds forward.

An interval the picker does not offer is still honored, and still shown: the form
adds it to the list rather than rounding it, so a hand-set 47 minutes is not quietly
overwritten by the next unrelated save.

## See also

- [Configuration](../reference/configuration.md) — the floor, the ceiling, and the
  failure threshold
- [Architecture](../explanation/architecture.md#adaptive-polling) — why the interval
  is learned, and the order the settings resolve in
- [CLI](../reference/cli.md#post-feedsidedit--change-one-subscription) — the form
  fields and what each does to the schedule
- [Data model](../reference/data-model.md#feeds) — every column referenced here
