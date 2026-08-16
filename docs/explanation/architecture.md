# Architecture

This describes the shape of the system and why it has that shape. For what the
commands and settings *are*, see [CLI](../reference/cli.md) and
[Configuration](../reference/configuration.md).

## One binary, several roles

`tome` is a single Go binary with subcommands. Two of them are long-running:

- `tome serve` — the HTTP surface: web UI, Fever API, health endpoints.
- `tome worker` — the job pool: feed polling, fetching, extraction, assets.
  *(M1.)*

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

At M0 there are no dependencies to check, so `/readyz` reports ready as soon as
the process is serving. The mechanism exists now, unused, because retrofitting
readiness onto a service that has been running without it means discovering
every place that assumed it was always ready.

## What runs where

```
                    ┌──────────────┐
   feeds ──poll──▶  │  worker      │ ──enqueue──┐
                    │  (M1)        │            │
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
                    │  blob store  │  filesystem (M3)
                    └──────────────┘
                           ▲
                    ┌──────┴───────┐
                    │  serve       │  web UI, Fever API, health
                    └──────────────┘
```

At M0, only `serve` exists, and only the health endpoints on it.
