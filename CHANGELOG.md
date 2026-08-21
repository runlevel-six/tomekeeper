# Changelog

Notable changes, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Every release is a git tag `vX.Y.Z`, and the container image published for it
carries **the same string**: `ghcr.io/runlevel-six/tomekeeper:v0.3.0` is the tag
`v0.3.0`, and `tome version` inside it says `v0.3.0`. One identifier, everywhere,
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
| **Major** | Reserved for 1.0. The Fever API landed in 0.2.0, so what is left is **multi-user** — after which this table stops having a caveat. | — |

"A patch release never migrates" is the load-bearing half, and it is enforced by
`scripts/check-release.sh` rather than remembered: it means a patch upgrade cannot
require anything but changing the tag.

An **extraction version bump** is called out under its own heading whenever it
happens, because it is the one change that wants a follow-up command
(`tome reextract`) to reach articles already in the archive.

## [Unreleased]

### Added

- **The failed-fetch queue now says which remedy a page wants.** Each row reports how much
  visible text the *served* page carried: a few hundred characters is a JavaScript shell
  that wants a browser, thousands is a page whose structure defeated the extractors and
  wants a CSS selector. Those have opposite fixes and both used to read as "extraction
  produced no content", so telling them apart meant running `tome explain` against a pod.
- **"Waiting" is a state.** An article whose domain is flagged for rendering when no
  browser is reachable stays retryable *and* says so — in the queue, and as a `waiting`
  badge in the reading list. It previously sat pending forever, retried every minute,
  invisible to the queue, and badged `queued` with the tooltip "the worker has not reached
  this page yet". It had.
- The domain-rules page explains what flagging a host for JavaScript actually does, and
  what happens when no browser is deployed. It used to say rendering did not exist.

### Changed

- **The headless browser now runs by default** (one replica, ~256Mi idle) instead of
  scaled to zero. Deliberately spending memory on a feature most archives never use,
  because the alternative is a checkbox that silently does nothing: the reader who flags a
  domain and the administrator who can scale a Deployment are not the same person, and
  multi-user widens that gap. Scale it to zero to turn it off — flagged articles then wait
  visibly rather than failing.

### Migrations

- **00008** adds `articles.page_visible_chars`. Nullable, because NULL means "not measured
  since this existed" and a default of zero would have claimed every article in the archive
  served an empty page.

## [v0.3.0] — 2026-08-20

Sites that build their pages in JavaScript can be archived. **This release adds a
migration**, so upgrading is the migration Job and then an apply — not a tag change.

### Added

- **Headless rendering**, for the sites that send an empty shell and build the article
  in JavaScript. A stock `chromedp/headless-shell` Deployment ships **scaled to zero**;
  the worker renders a page only when a domain rule flags the host `requires_js` *and* a
  browser is reachable, so either alone does nothing. `requires_js` has been a column an
  operator could set and nothing could read since the schema was written; it now works.
  See [Enable headless rendering](docs/how-to/enable-headless-rendering.md).
  - **Rendering happens at fetch time, not as a rung of the extraction ladder.** What
    gets stored is the DOM the browser built, so extraction stays offline and
    `tome reextract` improves rendered articles without re-fetching anything — which a
    rendering rung would have made impossible.
  - Renders run on their own River queue at `TOME_RENDER_CONCURRENCY` (default 1), so a
    page whose script never finishes cannot consume the pool that polls feeds.
  - Images, media and fonts are refused **by resource type**, the archive's own
    User-Agent is sent, and robots.txt is checked before the browser starts. The
    unavoidable residue — the page's own JavaScript runs — is documented rather than
    glossed over.
- `TOME_RENDER_BROWSER_URL` and `TOME_RENDER_CONCURRENCY`.

### Changed

- `tome explain` reports whether a page came through a browser, and labels the feed
  body's size as markup — the ladder measures its *text*, and two unlabelled counts on
  adjacent lines read as a contradiction.

### Fixed

- **A rejected feed body reported "0 characters" whatever its length.** The rung zeroed
  its result before the explanation was built, so a body that missed the 200-character
  floor by one looked identical to one with no text at all. It now reports what it
  measured — 134 characters, on the article that exposed it. Extraction output is
  unchanged, so no `extract.Version` bump.
- `tome explain` announced any lookup failure as "no article N", including a schema
  older than the binary. A missing column now says so.

### Migrations

- **00007** adds `articles.browser_rendered`. Recorded at fetch time rather than
  inferred from the domain rules in force when somebody asks, because rules change.

## [v0.2.0] — 2026-08-20

Mobile clients can read the archive. No migration, so upgrading is a tag change and
an apply — but see the note under Changed about `TOME_SESSION_KEY`.

### Added

- **The Fever API**, so mobile RSS clients can read the archive. `POST /fever/`,
  authenticated with `api_key` — MD5 of `username:password`, the credential the
  protocol specifies and the reason `users.api_key` has existed since the schema was
  written. Groups, feeds, items, the two id-list sync calls, and marking an item, a
  feed or a group read. What a client gets in the `html` field is the **extracted
  body**, not the summary the feed shipped, which is the entire point.
  See [Fever API](docs/reference/fever-api.md) and
  [Connect a mobile client](docs/how-to/connect-a-mobile-client.md).
- **Signed asset URLs.** `GET /assets/…` now accepts either a session, as before, or a
  `sig=` this service issued. A Fever client renders a body in its own view with no
  cookie, and an `<img>` tag cannot carry a POSTed credential, so without this every
  picture in every client is a broken image icon. The signing key is derived from
  `TOME_SESSION_KEY` with its own HKDF label, so there is nothing new to configure —
  but rotating that secret now invalidates outstanding image URLs along with every
  session, and clients recover by re-fetching bodies. URLs last 30 days.

No migration. `users.api_key` was already there.

### Changed

- `TOME_SESSION_KEY` now derives two independent keys rather than one. Generating one
  at startup, which happens when the variable is unset, therefore also invalidates
  synced image URLs on every restart — the startup warning says so.

### Fixed

- **An import of a truncated export could run forever.** Running out of input was
  treated as a recoverable per-record problem, and since a decoder at the end of its
  input cannot advance, the adapter reported `record N: unexpected EOF` with `N`
  climbing until the process was killed. Both adapters, both the CLI and the upload.
  It was latent rather than new: the standard library reports a truncation as one of
  two unrelated error values depending on where the cut falls, and which one it uses
  moved in Go 1.27, so the shape the fixtures held was the one that stayed fatal.
  Running out of input is now fatal in either shape, which is what the two-pass import
  has always promised.
- **A truncated export now says the file is incomplete rather than naming a record.**
  A file cut between records has no bad record to point at, and the number it landed
  on was one past the end — "record 3" of a two-record file, which sends whoever is
  holding a half-downloaded library looking for something that is not there.

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

[Unreleased]: https://github.com/runlevel-six/tomekeeper/compare/v0.3.0...HEAD
[v0.3.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.2.0...v0.3.0
[v0.2.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/runlevel-six/tomekeeper/releases/tag/v0.1.0
