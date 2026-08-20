# Architecture

This describes the shape of the system and why it has that shape. For what the
commands and settings *are*, see [CLI](../reference/cli.md) and
[Configuration](../reference/configuration.md).

## One binary, several roles

`tome` is a single Go binary with subcommands. Two of them are long-running:

- `tome serve` — the HTTP surface: the web interface, the Fever API that mobile
  clients sync against, and the health endpoints. The Fever credential is a column
  written when the password is set rather than derived on demand, because MD5 of the
  cleartext cannot be recovered from an argon2id hash — which is why that column
  predates the API that reads it.
- `tome worker` — the job pool: feed polling, fetching, extraction, image
  localization, and retention.

Beside them, and not built from this image at all, sits an optional third workload: a
stock headless browser. The worker talks to it over the DevTools protocol for the
domains an operator has flagged as needing JavaScript, and it ships **scaled to zero**,
because most archives never flag one. Two things follow from it being separate. Its
memory — about a gigabyte per page — is limited where it cannot take the worker down
with it; and rendering happens at *fetch* time rather than as a rung of the extraction
ladder, so what gets stored is the DOM the browser built and extraction stays offline,
re-runnable, and identical for every article. See
[Enable headless rendering](../how-to/enable-headless-rendering.md).

They are deployed as two workloads built from one image. The alternative — one
process doing both — is simpler to start and wrong to run: extraction is CPU-
and memory-hungry and bursty, while the reader UI needs to stay responsive.
Separating them means a backlog of ten thousand articles being re-extracted
cannot make the archive unreadable while it happens, and the two can be scaled
and restarted independently.

Keeping them in *one binary* rather than two is what makes that cheap. There is
one build, one image, one version, and no possibility of the server and the
worker disagreeing about the schema because someone deployed one and not the
other.

## One stateful dependency

PostgreSQL is the database, the job queue, and the search index.

This is a deliberate refusal of the usual arrangement — Postgres plus Redis for
queueing plus Meilisearch or Elasticsearch for search. Each of those is a
better component in isolation. Together they are three things to back up, three
things to upgrade, three things that can be the reason the archive is down, and
three things a future maintainer has to understand before they can safely
change anything.

The scale here does not justify that. A single reader's feeds produce a few
thousand jobs a day and a corpus in the low hundreds of thousands of documents.
Postgres does queues (via River) and full-text search (via `tsvector`)
perfectly well at that size.

Search sits behind a `SearchIndex` interface so that this can be revisited
without touching any calling code, if the archive ever outgrows it.

## Files are the archive

Extracted articles and their images are written to a filesystem tree of dated,
human-navigable directories, with relative links between them. Opening an
article's `index.html` from disk in a browser renders it, with images, with no
server running and no database present.

The database is an index over those files, and can be rebuilt from them.

This inverts the usual relationship, and it is the most important decision in
the project. A feed reader that dies takes nothing with it — you re-import your
OPML. An archive that dies takes ten years of reading with it. So the archive
is stored in the format most likely to still be readable in ten years, which is
"HTML files in folders", and the parts most likely to rot — the schema, the
binary, this codebase — are the parts you are allowed to lose.

## Startup and health

Configuration is validated once, before anything else, and the process refuses
to start if it is wrong. A service that starts with a bad database URL and
discovers it an hour later during a feed poll has converted an obvious failure
into a subtle one.

Liveness and readiness are separate endpoints answering genuinely different
questions:

- `/healthz` asks *is this process working*, and never consults a dependency.
- `/readyz` asks *should this instance receive traffic*, and consults all of
  them.

Conflating the two is a common and expensive mistake. If liveness checked the
database, then a brief Postgres restart would fail the probe on every replica,
the orchestrator would kill them all, and a recoverable dependency blip would
become a crash loop that outlasts the original problem. Readiness failing is
the correct response: stop sending traffic, keep the process alive, recover
when the dependency does.

`tome serve` registers two: the database connection, and whether the applied
schema version is the one this binary was built against.

The schema check is on readiness rather than startup because the image is
republished on every green build, so a pod restart can quietly pull a binary newer
than the database. Failing readiness with both version numbers and the remedy is
findable; refusing to boot would bury the same information in a restarting
container's logs. `tome worker` makes the opposite call and refuses to start
outright — a worker writing through a schema it does not understand does not fail
cleanly, it fails every job it picks up, burns the retries, and discards real work
while looking busy.

The blob root is deliberately not a check at all. If it cannot be opened the
reader loses images, not the interface, and the directory may simply not exist yet
because the worker has not run.

## Metrics are on a different port

Both long-running commands serve Prometheus metrics, and neither serves them on
the port the reader is on.

The reason is not tidiness. An Ingress routing `/` to `tome serve` would publish
whatever else that port answered — and the outbound metrics name every host the
archive fetches from, which is a reading list. A separate listener means exposing
the archive to the internet and exposing its metrics are two decisions instead of
one, and the second one has to be taken deliberately.

`TOME_METRICS_ADDR` set to empty disables it.

## Preferences live in the database, not in a cookie

The reader's palette is a column on `users`, read on every page render and written
into the `<html>` element server-side.

A cookie would have been less work and is what most applications do. It is wrong
here for a reason worth stating: a preference in a cookie is applied by
JavaScript after the document arrives, which means a visible flash of the wrong
palette on every load — and this is a reading tool, on a page someone opens
hundreds of times. Rendering it into the first paint costs one column and one
query that is already being made.

It also means the choice follows the reader to another browser, which is what
someone who set it once expects.

## What runs where

```
                    ┌──────────────┐
   feeds ──poll──▶  │  worker      │ ──enqueue──┐
                    │  scheduling  │            │
                    └──────────────┘            ▼
                                        ┌───────────────┐
                                        │  PostgreSQL   │
                                        │  + job queue  │
                                        └───────────────┘
                                            ▲       │
                    ┌──────────────┐        │       │
                    │  worker      │ ◀──────┴───────┘
                    │  extraction  │
                    └──────┬───────┘
                           │ writes
                           ▼
                    ┌──────────────┐
                    │  blob store  │  filesystem
                    └──────────────┘
                           ▲
                    ┌──────┴───────┐
                    │  serve       │  web interface, health
                    └──────────────┘
```

A poll records references and leaves articles at `fetch_status = 'pending'`;
fetching them is a separate job, so a slow or blocked site cannot hold up
ingest.

## Scheduling

Polling is split into two jobs rather than one.

`schedule_feeds` runs every minute and asks a single indexed question: which
feeds have `next_poll_at` in the past? It enqueues a `poll_feed` job for each,
up to a hundred at a time.

`poll_feed` does the network work for one feed.

The split matters because a poll is a network round trip that can take thirty
seconds against a slow origin, while the scheduling decision is microseconds.
One combined job would serialize every feed behind the slowest server in the
list. Separating them also means the concurrency limit applies where the
expense is, and a stuck feed delays only itself.

The batch limit exists for the first import: a few hundred feeds all default to
`next_poll_at = now()`, and without a bound the first scheduler run would
enqueue every one of them at once. Nothing is lost — whatever does not fit is
still due a minute later.

A `poll_feed` job is unique per feed while one is pending or running. Without
that, a scheduler run overlapping a slow poll would enqueue a second poll of the
same feed, and the two would race to write the same conditional-GET validators
while the origin server watched what looked a lot like hammering.

### Asking for a poll from the web interface

The **Check all feeds now** button on the Feeds page does not enqueue anything. It
sets `next_poll_at = now()` and lets the scheduler find the feeds a moment later.

That is a consequence of the split above rather than a limitation of it. `tome
serve` has no job client and wants none: giving the request path the ability to
insert polls would mean two processes able to enqueue the same work, and the
obvious next step — doing the fetch inline so the button could report a result —
would have a request handler holding a connection open while seventy origin
servers think about it. Nudging the schedule keeps polling entirely in the worker,
at the cost of "now" meaning "within a scheduler pass", which the page says out
loud.

The five-minute floor on that button lives in the store rather than the handler,
because it is a fact about how often a feed may be polled and not about who asked.

## Adaptive polling

A feed's interval is learned rather than configured. Each poll that finds
nothing multiplies the interval by 1.5, up to a 24-hour ceiling; each poll that
finds something halves it, down to a 15-minute floor. A feed that declares its
own cadence through the syndication module is believed instead, still clamped.

Halving rather than resetting to the floor is deliberate. Resetting means a
feed that posts once a day climbs the entire ladder back to the ceiling every
single day — about a dozen pointless requests per feed per day, which is
precisely the impoliteness the interval exists to avoid. Halving converges on
the feed's real cadence from both directions.

Failures back off exponentially from the floor, and a feed is disabled after
twenty consecutive failures. Disabled is not deleted: the feed keeps its last
error, and surfacing it is the point. A feed reader that quietly stops
collecting from a source is worse than one that stops loudly.

### When the reader would rather decide

Learned intervals are right for most feeds and wrong for the ones somebody cares
about the timing of. A feed that publishes twice a year climbs to the ceiling and
stays there, which is correct until the week the reader is waiting on it. So there
are two explicit settings: a general cadence on **Settings**, and an override on any
one feed's **Edit** form.

The order is: the feed's own override, then the reader's general cadence, then a
cadence the feed declares for itself, then the learned interval.

A reader's choice beating the feed's own `sy:updatePeriod` is the arguable one — the
publisher does know their schedule. It is settled that way because an explicit
setting whose effect depends on markup the reader cannot see is not a setting: asking
for hourly checks on a feed that declares itself daily would get daily, with nothing
said and no way to find out why. The declaration is still better information than the
estimate, so it keeps its place ahead of the learned interval.

Two bounds survive a reader's choice, and the asymmetry between them is the point:

- **The floor holds.** `TOME_POLL_MIN_INTERVAL` is a promise made to other people's
  servers, and a dropdown cannot spend somebody else's request budget. A shorter
  choice is raised to it.
- **The ceiling does not apply.** It exists only to stop this service polling a quiet
  feed for nothing, so a reader asking for *less* often than the ceiling — weekly, say
  — is asking for something there was never a reason to refuse.

Failure backoff is not shortened by a chosen cadence either, but the cadence is a
floor on it. Backoff starts from the 15-minute floor, so without that a feed set to
weekly would be polled every 15 minutes the moment it broke: hundreds of times more
often when it is failing than when it works.

Choosing a shorter cadence also moves the next poll, because otherwise the choice
would not take effect until the poll it was meant to replace — up to a day later,
which presents as a setting that did nothing. It moves it to `last poll + the new
interval` rather than to `now()`: that is the difference between a cadence and the
**Check all feeds now** button, and it means a list of seventy feeds settles into the
new rhythm instead of arriving on the worker at once.
