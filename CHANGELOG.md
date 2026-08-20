# Changelog

Notable changes, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Every release is a git tag `vX.Y.Z`, and the container image published for it
carries **the same string**: `ghcr.io/runlevel-six/tomekeeper:v0.1.0` is the tag
`v0.1.0`, and `tome version` inside it says `v0.1.0`. One identifier, everywhere,
so "what is running" has a single answer. See
[Cut a release](docs/how-to/cut-a-release.md).

## What the numbers promise

While the major version is `0`, the public interface is the HTTP routes, the CLI,
the environment variables, and the archive on disk — not the Go packages, which are
internal.

| Bump | Means | Upgrading |
|---|---|---|
| **Patch** (`0.1.0` → `0.1.1`) | Fixes only. **Never a database migration.** | Change the tag and apply. Nothing else to do. |
| **Minor** (`0.1.0` → `0.2.0`) | Features, and any release that adds a migration. May change defaults or remove a flag, with the removal noted here. | Run the migration Job, then apply. |
| **Major** | Reserved for 1.0, which is when multi-user and the Fever API land and this table stops having a caveat. | — |

"A patch release never migrates" is the load-bearing half, and it is enforced by
`scripts/check-release.sh` rather than remembered: it means a patch upgrade cannot
require anything but changing the tag.

An **extraction version bump** is called out under its own heading whenever it
happens, because it is the one change that wants a follow-up command
(`tome reextract`) to reach articles already in the archive.

## [Unreleased]

Nothing yet.

## [v0.1.0] — 2026-08-20

First tagged release. Everything below has been running against a real archive of
about 2,100 articles from 66 feeds (2,131 at the time of writing).

### Added

- **Feeds.** RSS, Atom and JSON Feed polling with conditional GET, per-feed
  adaptive intervals between 15 minutes and a day, exponential backoff, and
  auto-disable after 20 consecutive failures. OPML import from the CLI or by
  upload. Add a feed by pasting a site address — it follows the page to the feed
  it advertises and reports what it carries before you subscribe. Edit, unsubscribe,
  sort and filter the list; check every feed now rather than waiting.
- **Choosing how often feeds are checked.** A general cadence in Settings and an
  override on any one feed, with the feed's own setting winning. Neither can poll
  more often than `TOME_POLL_MIN_INTERVAL`.
- **Archiving.** Every article's page is fetched, the readable body extracted
  through a five-rung ladder, images localized and transcoded, and the result
  written to disk as a standalone page that opens in a browser with nothing
  running. Bodies are immutable and versioned; `tome reextract` re-runs extraction
  over stored pages without touching any site.
- **Reading.** Unread, everything, per-feed, per-category and per-tag lists with
  keyset pagination, full-text search across the archive, starring, saving,
  highlights, an attention queue for what did not come through cleanly, and
  mark-as-read on scroll. Six palettes plus a neutral default. Installs to a home
  screen and draws its own navigation there.
- **Typography.** Literata for prose and Inter for the interface, both shipped with
  the binary rather than named in a stack.
- **Politeness.** Per-host rate limiting, `robots.txt` honored for article and
  asset fetches (and deliberately not for feed polls), an honest `User-Agent`, and
  `Retry-After` obeyed inline up to five seconds.
- **Import and export.** A Wallabag library reads in; the whole archive writes out
  as one file the importer reads back.
- **Operations.** Kubernetes manifests including PostgreSQL, a migration Job, a
  nightly `pg_dump` CronJob and an `await-schema` initContainer; Docker Compose for
  a single machine; Prometheus metrics on a port that is deliberately not routed;
  `/healthz` and `/readyz` that distinguish liveness from readiness; and a schema
  check that refuses to serve against a database older than the binary.

### Known gaps

- **No Fever API yet**, so mobile clients cannot connect. Planned for 0.2.
- **Single user.** The schema is user-scoped throughout, but nothing creates a
  second account.
- **Blob replication is manual.** The nightly dump covers the database; the archive
  directory is yours to replicate. See
  [Back up and restore](docs/how-to/back-up-and-restore.md).
- **JavaScript-rendered sites are not archived.** No headless browser, by choice.

[Unreleased]: https://github.com/runlevel-six/tomekeeper/compare/v0.1.0...HEAD
[v0.1.0]: https://github.com/runlevel-six/tomekeeper/releases/tag/v0.1.0
