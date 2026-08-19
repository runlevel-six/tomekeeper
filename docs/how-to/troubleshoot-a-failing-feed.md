# How to troubleshoot a failing feed

A feed that stops producing articles is either failing to fetch, failing to
parse, or genuinely quiet. This tells them apart.

## Find failing feeds

Start on the **Feeds** page in the web interface. It lists every subscription with
its unread count, when it last succeeded, and — for anything failing — the failure
count and the last error verbatim. A banner at the top says how many feeds are
failing, because a slow puncture in the archive is the thing worth noticing.

Two controls make that list usable once there are more than a screenful:

- **Showing → failing only** hides everything healthy. `disabled only` is a separate
  choice, because a feed that has stopped being polled is its own state rather than
  the worst end of failing.
- **Health** as a sort puts the worst first, and **Last success** sorted oldest-first
  finds feeds that are not failing at all and have simply gone quiet.

The banner keeps counting the whole archive while a filter is applied, so filtering
cannot hide how many feeds are broken.

That answers "which feeds are broken and why" without a database client. The SQL
below is for the parts the page does not do: comparing a poll against the items it
produced, and the surgery in the last three sections. Connect with
`psql "$TOME_DATABASE_URL"`.

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
| `HTTP 404` | The feed moved or was removed. | Find the new URL and correct it with **Edit** on the feed's row — see [below](#when-the-feed-is-fine-but-the-url-was-wrong). |
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
(default 20). Fix the cause first, then press **Edit** on its row in **Feeds** and
check **Check this feed on a schedule**. Saving clears the failure count and the last
error and queues the feed, and the worker picks it up within a minute.

Clearing `consecutive_failures` is the part that matters, and the form does it for
you: without it the next single failure re-crosses the threshold and the feed is
disabled again immediately.

The same form turns a feed *off* by hand — for a site that has gone bad rather than
broken. That keeps the failure count and the error, so the row can still say what
went wrong, and it leaves everything already archived from the feed exactly where it
is.

By hand, or for a whole run of feeds at once:

```sql
UPDATE feeds
SET disabled = false, consecutive_failures = 0, last_error = NULL,
    next_poll_at = now()
WHERE id = $1;
```

## Watch a poll happen

```sh
TOME_LOG_LEVEL=debug tome worker
```

Every poll logs one line with the item counts and the next interval; failures
log the cause. Health probes are logged at `debug` too, so this is noisy — it
is a diagnostic mode, not a default.

To force a poll now rather than waiting for the schedule, press **Check all feeds
now** on the Feeds page. It brings every enabled feed forward and the worker picks
them up within a minute.

Two things it will not do, both on purpose. It leaves alone any feed polled in the
last five minutes — so pressing it twice while watching a debug log looks like
nothing happening, and the page says how many were held. And it does not revive
disabled feeds; that is the section above.

A feed you have just re-enabled or given a new address is queued by that save
already, so it needs nothing here. For one specific feed otherwise:

```sql
UPDATE feeds SET next_poll_at = now() WHERE id = $1;
```

## When the feed is fine but the URL was wrong

Press **Edit** on the feed's row and correct the address. Saving keeps the
subscription — its poll history, its category, and every article already archived
under it — which is the reason to edit rather than to subscribe again at the new
address and abandon the old row.

Two things saving does for you, both of which have to happen and neither of which is
visible in the row:

- **The validators are discarded.** An `ETag` from the old endpoint is meaningless to
  the new one, and sending it invites a `304` for content you have never actually
  seen — a feed that looks healthy and produces nothing.
- **The failure count and last error are cleared, and the feed is queued now.** They
  described the old address. Left in place, a corrected feed would sit a few failures
  from being disabled for a fault that no longer exists.

## When the same feed is subscribed to twice

Moving a feed onto an address you already subscribe to is refused rather than merged,
because the two rows would be indistinguishable in the list. The refusal names the
other subscription and says whether *it* is the one that is working — which is the
usual shape of this: an OPML export carried both the old address and the new one, and
the row you are editing is the spare that has never fetched anything.

Remove the spare with **Unsubscribe** on its edit form. That deletes the subscription
and its `feed_items` — the record of which feed carried what — and no articles. They
are the root entity here, not children of a subscription, so everything archived stays
archived.

One consequence the confirmation spells out with a count: an article that *only* that
feed carried and that you never opened is no longer referenced by anything the
interface lists. It is still on disk and still in an export, and subscribing to the
feed again brings it back, bodies and images included. Anything you read, starred or
saved is unaffected either way — `article_state` keeps it reachable on its own.

Nothing sweeps those unreferenced articles up afterwards. Retention only releases the
bodies of articles somebody has *read* and then left alone, so an article nobody ever
opened is never expirable; it simply sits there. If you want the space back, that is a
`psql` job today.

For a whole run of subscriptions at once:

```sql
DELETE FROM feeds WHERE id = $1;
```

The equivalent edit, for a script or a whole run of feeds at once:

```sql
UPDATE feeds SET feed_url = 'https://example.com/feed.xml',
                 etag = NULL, last_modified = NULL,
                 consecutive_failures = 0, disabled = false,
                 next_poll_at = now()
WHERE id = $1;
```

## See also

- [Configuration](../reference/configuration.md) — polling and failure settings
- [CLI](../reference/cli.md#tome-worker) — what the worker does and when
- [Data model](../reference/data-model.md#feeds) — every column referenced here
