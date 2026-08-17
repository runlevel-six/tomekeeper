# How to troubleshoot a failing feed

A feed that stops producing articles is either failing to fetch, failing to
parse, or genuinely quiet. This tells them apart.

Until the feed health view exists, the answers come from SQL. Connect with
`psql "$TOME_DATABASE_URL"`.

## Find failing feeds

```sql
SELECT id, title, consecutive_failures, disabled,
       last_polled_at, last_success_at, last_error
FROM feeds
WHERE consecutive_failures > 0 OR disabled
ORDER BY consecutive_failures DESC;
```

`last_error` holds the most recent cause verbatim. A feed is never dropped
silently: the error survives even after the feed is disabled.

## Read the error

| `last_error` | What it means | What to do |
|---|---|---|
| `HTTP 404` | The feed moved or was removed. | Find the new URL and re-import, or unsubscribe. |
| `HTTP 403` | Blocked, often by a CDN that dislikes datacenter addresses. | Set `TOME_CONTACT_URL` so you are contactable, then ask the site. Do not build circumvention. |
| `HTTP 429 (Retry-After: …)` | Rate limited — you are asking too often. | Raise `TOME_POLL_MIN_INTERVAL`. The backoff already handles it, but a floor that respects the site is better manners. |
| `HTTP 5xx` | The site is broken, probably temporarily. | Nothing. Backoff will retry, and a success clears the counter. |
| `parsing feed: …` | The response is not a feed this parser recognizes. | Fetch the URL yourself — it is usually an HTML error page, a login wall, or a feed URL that now redirects to a homepage. |
| `response exceeds the … byte limit` | The feed is larger than 10MB. | Usually a feed with no item limit. There is nothing to configure; report it to the publisher. |
| `dial tcp … connection refused`, `no such host` | DNS or network. | Check from the same host the worker runs on. |

## Confirm whether it is really quiet

A feed can be healthy and produce nothing, which looks identical from the
outside. Compare the last success against the last item:

```sql
SELECT f.title,
       f.last_success_at,
       f.poll_interval,
       max(i.seen_at) AS last_item_seen
FROM feeds f
LEFT JOIN feed_items i ON i.feed_id = f.id
WHERE f.id = $1
GROUP BY f.id;
```

A recent `last_success_at` with an old `last_item_seen` is a working feed with
nothing to say. The `poll_interval` will have grown toward
`TOME_POLL_MAX_INTERVAL`, which is the adaptive interval doing its job — the
feed is not being neglected, it is being polled proportionately.

## Re-enable a disabled feed

A feed is disabled after `TOME_FEED_FAILURE_THRESHOLD` consecutive failures
(default 20). Fix the cause first, then:

```sql
UPDATE feeds
SET disabled = false, consecutive_failures = 0, last_error = NULL,
    next_poll_at = now()
WHERE id = $1;
```

The worker picks it up within a minute. Clearing `consecutive_failures` matters:
without it, the next single failure re-crosses the threshold immediately.

## Watch a poll happen

```sh
TOME_LOG_LEVEL=debug tome worker
```

Every poll logs one line with the item counts and the next interval; failures
log the cause. Health probes are logged at `debug` too, so this is noisy — it
is a diagnostic mode, not a default.

To force a poll now rather than waiting for the schedule:

```sql
UPDATE feeds SET next_poll_at = now() WHERE id = $1;
```

## When the feed is fine but the URL was wrong

Feed URLs are stored exactly as imported, and a subscription is keyed by
`(user, feed URL)`. Correcting a URL is therefore a new subscription rather than
an edit:

```sql
UPDATE feeds SET feed_url = 'https://example.com/feed.xml',
                 etag = NULL, last_modified = NULL,
                 consecutive_failures = 0, disabled = false,
                 next_poll_at = now()
WHERE id = $1;
```

Clear the validators when the URL changes. An `ETag` from the old endpoint is
meaningless to the new one, and sending it invites a 304 for content you have
never actually seen.

## See also

- [Configuration](../reference/configuration.md) — polling and failure settings
- [CLI](../reference/cli.md#tome-worker) — what the worker does and when
- [Data model](../reference/data-model.md#feeds) — every column referenced here
