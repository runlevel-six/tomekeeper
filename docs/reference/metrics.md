# Metrics

Prometheus metrics, served by both `tome serve` and `tome worker`.

## Where they are

**On their own port, not on the web interface.** `TOME_METRICS_ADDR` defaults to
`:9090`; setting it empty disables the endpoint entirely.

This is a privacy boundary rather than a layout preference. An Ingress in front of
the reader routes `/`, so an endpoint on the main server would be reachable from
the public internet — and `tome_outbound_responses_total` names every host the
archive fetches from, which is a published list of what you read. Keep the metrics
port unrouted and scrape it from inside the cluster.

Both processes publish. They are separate deployments with separate failure modes,
and the numbers that matter most — poll outcomes, extraction, every outbound
request — are made in the worker.

## Where the numbers come from

Most are read from PostgreSQL when Prometheus scrapes, not counted as events
happen.

The archive's interesting quantities are already facts in the database: how many
feeds are failing, how many articles have no body, how deep the queue is. Counting
them again in the application would create a second source of truth that drifts,
and that resets to zero on every restart. A gauge read from the table it describes
cannot be wrong.

The exception is outbound HTTP, which leaves no row behind when it succeeds and no
row at all when it fails. Those are counters.

## The metrics

### Read from the database

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `tome_feeds` | gauge | `state` | Subscriptions by health: `ok`, `failing`, `disabled`. |
| `tome_articles` | gauge | `fetch_status` | Articles by fetch outcome: `pending`, `ok`, `failed`, `skipped`. |
| `tome_articles_by_assets_status` | gauge | `assets_status` | Articles by image localization: `pending`, `ok`, `partial`, `none`. |
| `tome_bodies` | gauge | `extractor` | Current bodies by which rung of the ladder produced them. |
| `tome_assets` | gauge | — | Distinct images stored. |
| `tome_asset_bytes` | gauge | — | Total bytes of stored images. |
| `tome_body_bytes` | gauge | — | Total bytes of stored bodies, HTML and text. |
| `tome_jobs` | gauge | `state` | Background jobs by River state. |

There are deliberately **no per-feed labels.** Which feed is failing is a question
the feed health page answers better, and a label carrying a feed title would put
your subscription list into the monitoring system.

### Counted in the application

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `tome_outbound_responses_total` | counter | `host`, `status` | Responses from remote sites. |
| `tome_outbound_failures_total` | counter | `host` | Requests that never produced a response. |

`status` is a class — `2xx`, `3xx`, `4xx`, `5xx` — except `429`, which keeps its
own bucket because it is the one status that means *you are being told to slow
down*, and averaging it into `4xx` hides exactly the thing worth seeing.

Counted per attempt rather than per call: for a site that is rate-limiting, the
interesting number is how many 429s it sent, not how many of your calls eventually
succeeded anyway.

### About the scrape itself

| Metric | Type | Meaning |
|---|---|---|
| `tome_scrape_errors_total` | counter | Queries that failed while collecting. Non-zero means some gauges above are missing, not that the archive is broken. |
| `tome_scrape_duration_seconds` | gauge | Time spent querying. |

Plus the standard Go runtime and process collectors: `go_goroutines`,
`go_memstats_*`, `process_resident_memory_bytes` and friends. They are the first
thing anyone asks for when a worker is behaving oddly.

A failing database degrades the scrape rather than breaking it — the process
metrics still report, and `tome_scrape_errors_total` says the rest did not.

## Things worth alerting on

Nothing here is prescriptive, but these are the shapes that mean something:

- `tome_feeds{state="disabled"}` going above zero. A feed is only disabled after
  sustained failure, and it will never recover on its own.
- `rate(tome_outbound_responses_total{status="429"}[1h])` rising. Somewhere you are
  being asked to slow down, and the fix is a `--rate` on a domain rule.
- `tome_articles{fetch_status="pending"}` growing without bound. Fetches are being
  enqueued faster than they complete, or the worker is not running.
- `tome_jobs{state="retryable"}` climbing. Work is failing repeatedly.
- `tome_scrape_errors_total` increasing, which usually means the database is
  unreachable from the process being scraped.

## See also

- [Configuration](configuration.md) — `TOME_METRICS_ADDR`
- [CLI](cli.md) — the health endpoints, which are a different thing and are on the
  main port on purpose
