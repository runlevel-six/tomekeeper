# CLI

Tomekeeper is a single binary, `tome`, with subcommands. Every subcommand reads
its settings from the environment; see [Configuration](configuration.md).

```
tome <subcommand>
```

Running `tome` with no subcommand prints usage to stderr and exits `2`.

## Subcommands

### `tome serve`

Runs the HTTP server in the foreground until it receives `SIGINT` or `SIGTERM`.

Takes no flags or positional arguments. Passing any argument is an error and
exits `2`; this is deliberate, so that a flag someone expects to exist fails
visibly instead of being ignored.

On a termination signal the server stops accepting new connections and waits up
to `TOME_SHUTDOWN_TIMEOUT` for in-flight requests to complete. A clean shutdown
exits `0`. If the timeout expires with requests still in flight, the failure is
logged at `error` and the process exits `1`.

**Required configuration:** `TOME_DATABASE_URL`.

Serves the web interface as well as the health endpoints. Every page except the
sign-in form and `/static/` requires a session; a request without one is
redirected to `/login`, or answered `401` if its `Accept` header asks for
something other than HTML, so an API client is not handed a login page with a
`200` on it.

Signing in is impossible until a password exists — see `tome migrate` below. The
sign-in page says so explicitly rather than reporting a wrong password, because
the fix is an operator action.

### `tome worker`

Runs the background job pool in the foreground until it receives `SIGINT` or
`SIGTERM`.

Takes no flags or positional arguments.

The worker polls feeds. It runs as a separate process from `tome serve` because
polling, extraction, and image processing are bursty and memory-hungry, and a backlog
must not be able to make the reader unresponsive. Both are built from the same
image.

Seven job types run:

| Job | Trigger | Work |
|---|---|---|
| `schedule_feeds` | every 60s, and once at startup | Selects up to 100 feeds where `next_poll_at <= now()` and `NOT disabled`, and enqueues a poll for each. |
| `poll_feed` | enqueued by the scheduler | Conditional GET, parse, upsert articles and references, enqueue a fetch per new article, update polling state. |
| `schedule_fetches` | every 60s, and once at startup | Enqueues a fetch for up to 100 articles still at `fetch_status = 'pending'`. In steady state it finds nothing; it exists for backlogs and for jobs lost to a crash. |
| `fetch_article` | enqueued per new article | Fetches the page subject to robots.txt and rate limiting, stores the gzipped original in the blob store, enqueues extraction. |
| `extract_article` | enqueued after a fetch, or by `tome reextract` | Runs the extraction ladder over the stored page. Touches no network. |
| `schedule_assets` | every 60s, and once at startup | Enqueues localization for up to 100 articles still at `assets_status = 'pending'` **that have a current body**. Articles with no body are settled to `none` at failure time rather than being left here unreachable. |
| `localize_assets` | enqueued after extraction | Downloads the article's images with the article as `Referer`, downscales and transcodes them, rewrites the body to point into the archive, and writes `index.html` and `meta.json`. |

Every job is unique per subject while one is pending or running, so a slow poll
cannot be overtaken by the next scheduler run, and three feeds carrying the same
story do not each fetch the page.

Per-domain rate limits are read from `domain_rules` once at startup. A rule
added later takes effect on the next restart.

On a termination signal the worker stops accepting new jobs and lets running
ones finish. A poll killed mid-write would leave a feed's stored validators
inconsistent with what was actually ingested, and the next poll would then take
a 304 for a feed whose items were never stored.

**Required configuration:** `TOME_DATABASE_URL`, and a writable
`TOME_BLOB_ROOT`. Behavior is shaped by `TOME_WORKER_CONCURRENCY`,
`TOME_POLL_MIN_INTERVAL`, `TOME_POLL_MAX_INTERVAL`,
`TOME_FEED_FAILURE_THRESHOLD`, `TOME_FETCH_RPS`, `TOME_FETCH_CONCURRENCY`, and
`TOME_CONTACT_URL`.

### `tome migrate`

Applies pending database migrations, then creates or renames the single v1 user
from `TOME_USERNAME`. Both are idempotent, so running it on every deployment is
correct.

Takes no flags or positional arguments.

Migrations never run automatically when `serve` or `worker` starts. They run
here, as their own command, so that a rollout can be gated on them completing
and so that two replicas starting at once cannot race each other.

Two migration histories are applied: the application schema, embedded in the
binary from `internal/db/migrations/`, and River's own job-queue schema, which
River owns and versions itself.

It also sets the user's password when `TOME_PASSWORD` is present, storing an
argon2id hash and deriving the Fever API key from the same cleartext. `serve`
never reads `TOME_PASSWORD`; it authenticates against the stored hash, so the
secret belongs to this command alone.

Setting a password always rotates the Fever API key. That is unavoidable rather
than unfortunate: the key is MD5 of `username:password`, which cannot be
recovered from the hash, so it has to be recomputed while the cleartext exists.
Mobile clients therefore need reconnecting after any password change.

On success it prints the seeded user and exits `0`:

```console
$ tome migrate
schema up to date; user "tome" is id 1
no TOME_PASSWORD set, so no password was changed
the web interface cannot be signed into until one is
```

```console
$ TOME_PASSWORD=... tome migrate
schema up to date; user "tome" is id 1
password set for "tome"
the Fever API key changed with it; mobile clients will need reconnecting
```

### `tome import-opml`

Adds subscriptions from an OPML file exported by another feed reader.

The web interface does the same thing under **Feeds → Import subscriptions**,
sharing this command's import logic rather than reimplementing it. Use this one
when the file is somewhere a browser is not, or when you want `--dry-run`.

```
tome import-opml [--dry-run] <file.opml>
```

| Flag | Description |
|---|---|
| `--dry-run` | Parse the file and print what would be imported. Touches no database and needs none, so it works before the service is set up. |

An outline with an `xmlUrl` is a feed; an outline without one is a folder whose
name becomes the category of everything beneath it. Nested folders are joined
with `/`, so a feed in `News` → `Local` gets the category `News/Local`.
Bookmarks, empty folders, and URLs that are not HTTP or HTTPS are skipped.

Feed URLs are stored exactly as the exporting reader wrote them. They are
deliberately **not** canonicalized: canonicalization is tuned for article links,
where a `ref` or `source` parameter is tracking noise, but on a feed endpoint
the same parameter may select which feed is served.

Re-running an import is safe. Subscriptions are keyed by `(user, feed URL)`, so
a second run updates titles and categories, creates no duplicates, and does not
disturb polling state — stored validators, intervals, and failure counts are
left alone.

The user is selected by `TOME_USERNAME` and must already exist; run `tome
migrate` first.

```console
$ tome import-opml --dry-run subscriptions.opml
subscriptions.opml: 7 subscriptions (dry run, nothing written)

CATEGORY    TITLE                FEED URL
Technology  Example Engineering  https://engineering.example.com/feed.xml
...

$ tome import-opml subscriptions.opml
subscriptions.opml: 7 added, 0 already subscribed
```

A subscription that fails to store is reported on stderr and the import
continues; the command exits `1` if any failed, so one bad entry costs neither
the other four hundred nor your awareness of it.

### `tome reextract`

Queues re-extraction of stored pages at the current extractor version. Makes no
requests to any site.

```
tome reextract [--target-version V] [--domain HOST] [--limit N] [--dry-run]
```

| Flag | Default | Description |
|---|---|---|
| `--target-version` | the compiled-in version | Select articles whose current body came from a version **other than** this — that is, the version you want everything brought *to*, not the version it is at now. The default is almost always what you want: after upgrading, a bare `tome reextract` reprocesses everything the new build would extract differently. Pass `0` to select everything, which is what you want after adding a domain rule. |
| `--since-version` | — | Deprecated alias for `--target-version`. The name reads as an ordering and is not one; passing the version your bodies are already at selects nothing and reports success. |
| `--domain` | every host | Restrict to one host and its subdomains. `example.com` covers `blog.example.com`, matching how a domain rule applies. |
| `--limit` | `0` (no limit) | Stop after queueing this many articles. |
| `--dry-run` | off | Count without queueing. |

Two kinds of article are never selected: bodies flagged `immutable`, which are
excluded by the query rather than skipped in a loop, and articles with no
stored page, which have nothing to re-extract from.

The command only queues. `tome worker` does the work, so a reprocess of the
whole archive competes with normal polling rather than monopolizing the machine,
and it survives a restart because the queue lives in Postgres.

See [Reprocess the archive](../how-to/reprocess-the-archive.md).

### `tome domain-rule`

Manages per-domain extraction overrides.

```
tome domain-rule list
tome domain-rule show <domain>
tome domain-rule set [flags] <domain>
tome domain-rule rm <domain>
```

Flags for `set`:

| Flag | Description |
|---|---|
| `--selector <css>` | CSS selector for the article body. Extraction uses it instead of the heuristics, and it overrides the ratio check. |
| `--strip <css>` | Selector removed before extraction. Repeatable. |
| `--rate <rps>` | Per-host request rate, overriding `TOME_FETCH_RPS`. |
| `--requires-js` | Marks the domain as needing a headless render. No effect until headless rendering exists. |
| `--notes <text>` | Why the rule exists. |

Rules apply to subdomains: a rule for `example.com` covers `blog.example.com`
unless that subdomain has a rule of its own, in which case the more specific one
wins. `show` names which rule matched, so an inherited one is not a surprise.

Flags must precede the domain: parsing stops at the first non-flag argument, so
`set example.com --selector …` prints usage rather than saving a rule.

A rule changes nothing already stored until the affected articles are
reprocessed, and `set` prints the command to do it. It needs both flags:

```sh
tome reextract --target-version 0 --domain example.com
```

`--target-version 0` because `reextract` selects on extractor version, so a bare
run finds nothing when every body is already current. `--domain` because the rule
can only affect that one site, and reprocessing a large archive to correct a
handful of articles is hours of needless work.

Rules are global and admin-only. How to extract a site's articles is a technical
fact about that site, identical for every reader.

See [Add a domain rule](../how-to/add-a-domain-rule.md).

### `tome archive`

Reports on what the archive holds.

```
tome archive stats
```

Counts articles, bodies, and images; reports bytes and the deduplication
saving; and estimates cost per thousand articles. Raw pages live on the
filesystem rather than in the database, so the command prints the `du` commands
that size them rather than guessing.

This exists because the acceptance criterion asks for storage across 1,000
real articles to be measured and recorded. See [Storage
layout](storage-layout.md#measuring-your-archive).

### `tome version`

Prints the build identity to stdout and exits `0`.

```
tomekeeper v0.1.0 (a1b2c3d) built 2026-08-16T23:00:03Z go1.26.5 linux/amd64
```

Version, commit, and build date are injected at link time. When they are not —
a `go build` with no flags, or `go run` — they are recovered from the Go build
info embedded from the git work tree, and a build with uncommitted changes is
reported with a `-dirty` suffix.

### `tome help`

Prints usage to stdout and exits `0`. `tome -h` and `tome --help` are
equivalent.

## HTTP endpoints

Served by `tome serve`. Both respond to `GET` and `HEAD`; any other method
returns `405`. Both set `Cache-Control: no-store` and return
`application/json; charset=utf-8`.

## Web interface

Served by `tome serve`. Every route except `/login` and `/static/` requires a
session.

| Route | Page |
|---|---|
| `GET /` | Unread stream |
| `GET /all` | Everything in the archive |
| `GET /starred` | Starred articles |
| `GET /saved` | Reading list — pages saved by hand |
| `GET /categories` | The categories the feeds are filed under |
| `GET /categories?name=` | One category's articles. Present-but-empty selects the feeds with no category. |
| `GET /articles/{id}` | The reader. Opening an article marks it read. |
| `GET /search?q=` | Search results |
| `GET /feeds` | Feed list, health, and tags |
| `GET /feeds/{id}` | One feed's articles |
| `GET /tags/{id}` | One tag's articles |
| `GET /attention` | Articles that did not come through cleanly |
| `GET /settings` | Palette and preferences |
| `GET /mark-read?from=` | Asks before marking a whole list read. `from=` names the list. |
| `POST /mark-read` | Marks everything unread in one list read. Same `from=`. |
| `POST /articles/{id}/read` | Mark read or unread. `on=true` or `on=false`. |
| `POST /articles/{id}/star` | Star or unstar. Same form field. |
| `POST /articles/{id}/keep` | Keep permanently, or stop. Same form field. |
| `POST /save` | Save a page by hand. `url=`. |
| `POST /feeds/import` | Subscribe to everything in an uploaded OPML file |
| `POST /feeds/refresh` | Bring every enabled feed forward to due |
| `POST /settings` | Save preferences |
| `GET /login`, `POST /login`, `POST /logout` | Session |
| `GET /assets/…` | Archived images, from `TOME_BLOB_ROOT` |
| `GET /static/…` | Stylesheet, keyboard script, logo, vendored htmx |

The state-changing `POST` routes take the state they should end in rather than
toggling, so a repeated or retried request is idempotent instead of landing back
where it started. They return the refreshed control alone, which is what htmx swaps
in; without JavaScript the same forms submit normally and the page reloads.

An article a reader cannot see is `404`, never `403`. A distinct "forbidden" would
confirm the article exists, which is exactly what the scoping discipline says one reader must not be
able to infer about another's archive.

### `POST /feeds/refresh` — check the feeds now

Sets `next_poll_at = now()` on every one of the reader's enabled feeds, so the
worker's scheduler picks them up on its next pass — within one `ScheduleInterval`,
which is a minute. It fetches nothing itself: `tome serve` has no job client, and
polling belongs to the worker.

Three things it deliberately does not do:

- **Feeds polled within the last five minutes are left alone.** The button is one
  tap and there are dozens of origin servers behind it. The page reports how many
  were held rather than hiding the fact.
- **Disabled feeds are not revived.** Re-enable them from the feed list; a refresh
  that quietly undid auto-disable would undo the feature on every reload.
- **It does not wait.** The page says the worker will get to them, because claiming
  otherwise would be a spinner that finishes before any feed has been contacted.

### `GET`/`POST /mark-read` — mark a whole list read

Scoped to the list on screen, never to the archive. `from=` carries the same list
token an article link does (see the table below), and the mark applies exactly the
query that drew that list: marking **Comics** read marks Comics, and marking
**Unread** read marks what is unread.

Two requests rather than one. The `GET` renders the list again with a confirmation
that names the count and the list; the `POST` does the work and re-renders with what
it marked. That is not ceremony — it is the one control here that is not its own
inverse. Every other button on a row can be pressed again to undo it; this one can
only be undone article by article, so the count goes in front of the reader first.
A confirmation page rather than a scripted dialog, because the content security
policy has no `unsafe-inline` and nothing in this interface requires JavaScript to
do something a reader could otherwise do.

Which lists may be marked, and why it is a short list:

| List | Marked in bulk? | Why |
|---|---|---|
| Unread, Everything, a feed, a tag, a category | Yes | The list is a filter, and the filter is what gets applied. |
| Starred, Saved | No | Hand-picked, one article at a time; a bulk control over them answers a question nobody asked. |
| Search | **No** | Results are ranked against a query string, so the list carries no filter to scope a mark by — applying its empty query would mark the whole archive read from a page showing four results. |
| Attention | No | A worklist selected by fetch status, not a reading order. |

Anything else — an unknown token, a feed belonging to somebody else, a list that
must not be marked — is `404`, the same nothing in every case.

Two properties worth knowing:

- **It marks the list, not the page.** A stream page is 50 rows with a cursor; the
  mark ignores both, because a control that emptied only what was on screen would
  read as broken.
- **An article already read keeps its original read timestamp.** Only unread rows
  are touched. `read_at` is what [retention](retention.md) measures from, so a bulk
  mark that re-stamped everything would quietly extend the life of what the reader
  had already finished.

### Navigation between lists and articles

An article link carries the list it was opened from, as `?from=<token>`:

| Token | List |
|---|---|
| `unread`, `all`, `starred`, `saved` | The corresponding stream |
| `feed:{id}` | One feed |
| `tag:{id}` | One tag |
| `category:{name}` | One category, `{name}` verbatim |
| `search:{text}` | The search that found it |
| `attention` | The attention queue |

The reader uses it for two controls that the browser cannot supply: a way back to
the list, and previous/next *within that list*. Installed as a web app there is no
back button at all, and even in a browser the back button cannot say what the next
article is.

An unrecognized token — a hand-edited URL, or a feed that is not the reader's —
falls back to a way back to the unread list, and no previous/next. `search:` and
`attention` grant a way back but no previous/next: search results are ranked by
relevance and the attention queue is a worklist, and neither is a reading order.

Previous and next within the unread list admit articles read in the last 30
minutes. Opening an article marks it read, so without that the list would
rearrange itself the instant a reader arrived and "previous" would point off the
top of a list holding nothing they had seen.

### Keyboard

| Key | Action |
|---|---|
| `j` / `↓` | Next entry |
| `k` / `↑` | Previous entry |
| `o` / `Enter` | Open the selected entry |
| `n` | Next article (in the reader) |
| `p` | Previous article (in the reader) |
| `u` / `Escape` | Back to the list the article was opened from |
| `r` | Reload this page |
| `s` | Star or unstar |
| `m` | Mark read or unread |
| `/` | Search |
| `g` then `u` `a` `s` `f` `c` | Go to unread, everything, starred, feeds, categories |

Every one of these presses a control the page has already drawn, so a shortcut can
never reach somewhere there was no visible way to.

### Installed as a web app

`/static/manifest.webmanifest` declares `display: standalone`, so the app can be
installed to a home screen or a dock. That removes the browser's own chrome, which
is why the page draws the navigation it does:

- A **tab bar** fixed to the foot of the screen below 34rem — unread, categories,
  search, starred, saved — since there is no browser UI and a thumb reaches the
  bottom. The same links live in the header nav at wider widths, and are hidden
  from it below the threshold so each exists once in the document.
- A **reload control** in the header, which is a link to the page itself. There is
  no address bar, no reload button, and no pull-to-refresh in a standalone window.
- The **unread count leads the `<title>`**, as `(12) Unread — Tomekeeper`, and is
  mirrored onto the app icon with the Badging API where the platform has one. The
  count is per page load; nothing polls in the background.

### Response headers

HTML responses carry
`Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self';
img-src 'self' data:; connect-src 'self'; manifest-src 'self'; form-action 'self';
base-uri 'none'; frame-ancestors 'none'`.

Nothing is `unsafe-inline` and nothing is third-party. That is affordable precisely
because the script is vendored and the images are localized — a page needing a CDN
could not have a policy this tight.

`manifest-src` is there because `default-src 'none'` blocks the web app manifest
outright, and the symptom is not an error anyone sees: "add to home screen" simply
offers a generic icon and the wrong name.

Archived images are served `Cache-Control: public, max-age=31536000, immutable`.
They are content-addressed, so the bytes at a path genuinely never change; this is
the one place in the application where an immutable cache is a fact rather than a
hopeful guess.

### `GET /healthz` — liveness

Always returns `200` while the process is able to serve HTTP.

```json
{"status": "ok"}
```

It deliberately does not check the database or any other dependency. A liveness
failure causes the container to be killed, and killing every replica because a
dependency is briefly unreachable converts a recoverable outage into a crash
loop. Dependency state belongs to `/readyz`.

### `GET /readyz` — readiness

Returns `200` when every registered dependency check passes, `503` when any
fails. The whole probe is bounded at 3 seconds.

```json
{"status": "ready", "checks": {"database": "ok", "schema": "ok"}}
```

```json
{"status": "not ready", "checks": {"database": "connection refused", "schema": "ok"}}
```

`tome serve` registers two checks:

- **`database`** pings the connection pool. A failing database takes this instance
  out of the load balancer while leaving the process alive to recover.
- **`schema`** compares the applied migration version against the one this build
  needs, and fails readiness with both numbers and the remedy when they differ.
  It is a readiness check rather than a startup check on purpose: refusing to boot
  would mean a crash loop with the reason buried in a restarting container's logs.
  This matters because CI republishes `:latest` on every green build, so a pod
  restart can pull a binary newer than the schema with nobody having erred.
  (`tome worker` takes the stricter line and refuses to start, because a worker
  writing through a schema it does not understand is a data problem rather than a
  serving one.)

The blob root is deliberately **not** a check. A blob root that cannot be opened
costs the reader images, not the interface: the pages still work, the log says why,
and the worker may well create the directory on its next run.

The `checks` field is omitted entirely when no checks are registered.

Requests to both endpoints are logged at `debug` rather than `info`, so that
orchestrator probes do not dominate the log.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success, including a clean shutdown after a termination signal. |
| `1` | Runtime failure. The server could not bind its address, or shutdown exceeded its timeout. |
| `2` | Bad invocation or invalid configuration. Nothing was started. |

The distinction between `1` and `2` is part of the operational contract: a
supervisor can tell "you configured me wrongly" from "something broke" without
parsing log output. Configuration errors are written to stderr as plain text,
not through the structured logger, because the logger's own configuration is
among the things that may have just failed to validate.

## Not yet implemented

| Subcommand | Status |
|---|---|
| `tome import` / `tome export` | Planned. The intermediate representation they will use already exists — see [Export format](export-format.md). |
| `tome reindex` — rebuild the search index | Not currently needed: the search index is a generated column PostgreSQL maintains itself. |

Invoking either exits `2` as an unknown subcommand. They are named here so that
the absence is a documented gap rather than something you conclude from a failed
command.
