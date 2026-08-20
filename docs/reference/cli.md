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
| `render_article` | enqueued by `fetch_article` when the host's domain rule sets `requires_js` | Loads the page in a headless browser, blocking images, media and fonts, and stores the **rendered DOM** as the article's raw page. Runs on its own `render` queue. |
| `localize_assets` | enqueued after extraction | Downloads the article's images with the article as `Referer`, downscales and transcodes them, rewrites the body to point into the archive, and writes `index.html` and `meta.json`. |

Every job is unique per subject while one is pending or running, so a slow poll
cannot be overtaken by the next scheduler run, and three feeds carrying the same
story do not each fetch the page.

Renders run on a **separate queue** (`render`) with its own width,
`TOME_RENDER_CONCURRENCY`, defaulting to 1. The default queue carries everything else at
`TOME_WORKER_CONCURRENCY`. The split is not tidiness: a page whose JavaScript never
finishes holds a slot for the render timeout, and enough of those in the shared pool
would stop feeds being polled because of one site's script.

Rendering is off unless `TOME_RENDER_BROWSER_URL` is set *and* a domain rule flags a
host — either alone does nothing. With no browser reachable, flagged articles stay
`pending` and are retried rather than being marked failed, so scaling a browser up later
picks them up. See [Enable headless rendering](../how-to/enable-headless-rendering.md).

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

### `tome await-schema`

Blocks until the database carries the migrations this build needs, then exits `0`.
Migrates nothing.

| Flag | Default | Meaning |
|---|---|---|
| `--timeout` | `5m` | How long to wait before giving up and exiting `1`. |
| `--interval` | `2s` | How long to wait between checks. Must be positive. |

This exists to be an initContainer, and the deployment manifests use it as one on
both Deployments. `serve` and `worker` both refuse to run against a schema older
than the binary, which is correct and which makes a mis-ordered deploy expensive:
the worker enters `CrashLoopBackOff` and the server fails readiness with `503`, so
the Ingress loses its backend and the archive is down until somebody runs the
migration Job.

Kubernetes has no dependency ordering inside an apply — there is no way to say
"this Job before those Deployments" — so the ordering is expressed at runtime by
the pods that need it. With this in front of them, the same mistake produces pods
sitting in `Init` with the reason in their logs.

Every outcome except "current" is waited on rather than failed, the database being
unreachable included: Postgres still starting is the most common thing this sees
and it fixes itself. That is also why the migration Job's `wait-for-postgres`
container has no equivalent here — this covers both.

A schema *newer* than the binary needs is treated as current. That is a rollback in
progress, and the older binary's queries work against a superset schema.

```console
$ tome await-schema
schema version 6 is current for this build
```

Giving up names the remedy rather than the timeout:

```console
$ tome await-schema --timeout 30s
{"level":"INFO","msg":"waiting for the database schema","reason":"the database is at schema
 version 5 and this build needs 6; waiting for the migration to be applied","attempt":1}
{"level":"ERROR","msg":"gave up waiting for the database schema","reason":"...","waited":"30s",
 "remedy":"run the migration Job: `tome migrate`"}
```

The waiting line is logged when the reason changes, not once per attempt: at two
seconds apart a five-minute wait would be 150 identical lines.

### `tome healthcheck`

Asks a running server whether it is alive, exiting `0` when `/healthz` answers
`200` and `1` otherwise. `--addr` defaults to `TOME_HTTP_ADDR` then `:8080`;
`--timeout` defaults to `3s`.

For Docker and Compose, which can only exec a command — the image is distroless,
so there is no `curl` or `wget` inside it to run. Kubernetes makes HTTP probes
itself and does not need this.

Liveness, not readiness, matching what an orchestrator should restart on:
`/healthz` answers without consulting the database, so a Postgres restart does not
get every container killed alongside it.

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

### `tome import`

Imports a reading library exported from another system. Articles, not
subscriptions — `tome import-opml` is the one for feeds.

```
tome import [--dry-run] [--format NAME] <export-file>
```

| Flag | Description |
|---|---|
| `--dry-run` | Print the report and write nothing. |
| `--format` | The source format. Detected from the file when omitted. |

| Format | Source |
|---|---|
| `wallabag` | A Wallabag JSON export: **Settings → Export → JSON**. |

Formats are detected by the field names in the file's first record, not by its
name, because a browser saves an export as whatever it likes. A file nothing
recognizes is refused with the list of formats this build reads.

**The command always reads the file twice.** The first pass reports what
importing would do; only then does the second write. That costs one extra read of
a file measured in megabytes and buys two things: a truncated or corrupt export
fails before anything has been written, and nobody is surprised by what an import
did to a library they have been keeping for ten years. `--dry-run` simply stops
after the first pass.

Unlike `tome import-opml --dry-run`, this needs a database even for a dry run.
The three numbers that make the report worth reading — new, already imported,
already in the archive by another route — are all questions about the archive, and
a report that could not answer them would be a list of what is in a file you
already have.

```console
$ tome import --dry-run wallabag.json
wallabag.json: 385 records from wallabag (dry run, nothing written)

  new               385
  already imported  0
  duplicate URLs    0
  without a body    43   42 of these hold wallabag's own fetch-failure message; this archive will fetch them itself
  with images       201  2135 images to fetch and archive, 21 not usable addresses
  tags              0
  highlights        0

Images are fetched by the worker after the import, not now. Until it gets to
them, articles show their text with the images missing.

$ tome import wallabag.json
...
imported 385 articles: 341 bodies stored, 44 queued for fetching
```

#### What an import does to the archive

| | |
|---|---|
| **Deduplicates by canonical URL** | A record whose URL a feed already carried becomes another reference to that article, not a second copy. One article, one body, one set of images. |
| **Stores the body immutably** | `content_origin` is `import:wallabag` and `immutable` is true, because an imported body may be the only surviving copy of a page that is gone. `tome reextract` skips it and no later fetch replaces it. |
| **Marks it saved** | Imports appear under **Saved**, dated when the source saved them rather than when you imported them, so a ten-year library keeps its own chronology. Saved also means [retention](retention.md) never releases it. |
| **Queues a fetch anyway** | Every imported article is left at `fetch_status = 'pending'`, so this archive fetches the page for itself: the ones the source never held get a body, and the rest get their original page stored beside the imported one. |
| **Adds, never removes** | Tags and highlights are additive, and read and starred are OR-ed with what you already have. See below. |

#### Re-running an import

Safe, and the intended way to recover from one that stopped halfway. Records are
keyed on `(user, source, source id)` in `import_records`, and the record is
written *last* — so a run interrupted between an article and its record leaves
the record absent, and the next run finishes the job.

A second run does not:

- create duplicate articles, bodies, tags, or highlights;
- remove a tag you added here;
- take back read or starred state. Both are OR-ed with what is already stored, so
  an article imported unread and read here stays read.

A source record with no stable id still imports; it simply cannot be recognized
by id later, and deduplicates by URL instead.

#### What the report is telling you

**"without a body"** is usually larger than expected, and the annotation is the
important part. Wallabag writes a sentence of its own prose into the content field
when its fetch failed — *"wallabag can't retrieve contents for this article"* — and
an importer that took the field at face value would store that sentence as the
article's permanent, immutable body. Those records are imported with no body and
queued for this archive to fetch instead, which is an improvement on the library
rather than a loss from it. In the maintainer's own export it is 42 of 385.

**Images are the slow part, and they happen afterwards.** Wallabag only downloads
images when `download_images` is enabled on that instance; with it off — the
common case — every `<img>` in an imported body still points at the site it came
from. The import stores those references and the worker localizes them on its own
schedule, so an article can arrive readable but pictureless for a while. If the
exporting instance *did* download its images, they live inside that installation
and cannot be reached from here at all; the report says so, with a count.

**Unreadable records are counted and listed with their position**, and the rest
still import. A record in a 6MB single-line export cannot be found any other way.
A failure that ends the *file* — a truncation, or a syntax error that leaves the
parser's position unknown — stops the import instead, because everything past it
is unknown rather than merely broken.

The user is selected by `TOME_USERNAME` and must already exist; run `tome migrate`
first. The command exits `1` if any record failed to import.

### `tome export`

Writes the archive as a file `tome import` reads back.

```
tome export [--out FILE]
```

| Flag | Description |
|---|---|
| `--out` | Write to this file. Without it the export goes to stdout, so it composes: piping into `gzip`, into a bucket, or into another machine's `tome import` needs no temporary file. |

The summary goes to stderr when the export is going to stdout, so a pipe carries
the archive and nothing else.

```console
$ tome export --out archive.json
exported 385 articles to archive.json: 341 bodies, 0 tags, 0 highlights, 0 images referenced
44 articles carry no body: a fetch that failed, or a body retention released. The article, its metadata and your reading state are still exported.
```

**Images and stored pages are referenced, not included.** The file is the database's
half of the archive; `TOME_BLOB_ROOT` is the other half, and a complete backup is
both. The command says so whenever the export references any.

See [Export format](export-format.md) for the record, and for what a round trip
preserves exactly and what it does not.

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
| `--selector <css>` | CSS selector for the article body. Extraction uses it instead of the heuristics, and it overrides the ratio check. **Every matching element is used**, in document order, so a body split across several blocks is selected by naming the class they share. A comma-separated list selects more than one kind of element; an element inside another match is skipped rather than emitted twice. |
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

### `tome corpus`

Captures a real page into the private extraction corpus.

```
tome corpus add [--name NAME] <url>
```

The corpus is the regression suite for every extraction change, and the pages in it
are third-party content that does not belong in this repository — so they live in
the directory `TOME_TEST_CORPUS_DIR` names, and this is what puts them there.

It fetches the page with this archive's own client (same user agent, same rate
limits, same robots.txt handling), saves it exactly as fetched, runs the current
extractor over it, and writes a starter `.want` beside it with the headers filled in
and the assertions left to you — with the article's opening and closing sentences
quoted as comments, so choosing what to assert on means reading rather than hunting
through a browser tab.

```console
$ export TOME_TEST_CORPUS_DIR=~/tomekeeper-corpus
$ tome corpus add https://example.com/posts/one-that-reads-badly
saved ~/tomekeeper-corpus/example-one-that-reads-badly.html (153 KB)
extracted 2661 characters via trafilatura

Now edit ~/tomekeeper-corpus/example-one-that-reads-badly.want:
  - keep a few phrases from the middle and the end of the article
  - add ! lines for navigation, bylines or promotional text that must not appear
```

**A page extraction currently fails on is worth capturing.** The starter file says
so and leaves the expectation to you, and the corpus stays red until the page
extracts properly — which is the difference between a known problem and a forgotten
one.

The `!` lines earn their minute: a case that only asserts what should be present
still passes when an extractor starts dragging in the navigation.

See the README in `internal/extract/testdata/pages/` for the file format, and
[Reprocess the archive](../how-to/reprocess-the-archive.md) for applying an
extraction change once it is made.

### `tome explain`

Reports what each rung of the extraction ladder produced for one article, and
which threshold accepted or rejected it.

```
tome explain [--body] <article-id>
```

| Flag | Default | Description |
|---|---|---|
| `--body` | off | Also print the opening of the winning body, for checking that the right element was selected rather than merely a long one. |

Works entirely from the stored page: no requests, no network, and an answer for a
site that has since gone away or changed. That is one of the reasons the raw fetch
is kept.

It resolves the domain rule and the feed body exactly as the worker does, so the
report describes the extraction that actually happens rather than a hypothetical
one. `Explain` and `Extract` are one implementation for the same reason.

```console
$ tome explain 1267
article 1267: https://example.com/2026/08/a-post
  stored page: pages/ab/cd/abcdef.html.gz (129 KB uncompressed)
  fetch: failed — extraction produced no content

  RUNG         CHARS  WORDS  IMAGES  OUTCOME
  page         41904  0      0       measured: 41904 characters of visible text; a body under 2000 characters must be at least 25% of it (10476 characters)
  domain_rule  0      0      0       skipped: no rule for this domain
  trafilatura  0      0      0       rejected: produced nothing
  readability  0      0      0       rejected: produced nothing
  feed_body    0      0      0       skipped: the feed carried no body for this article
  page_images  0      0      0       rejected: no image on the page is named after this article's slug or its title, so none of them is its content

Result: no body. no extractor produced acceptable content
…
```

The `page` row is the denominator: it is the visible text of the whole document,
which the ratio check measures a short body against. A rung that never ran says
`skipped` and why, because "readability was not tried" is an answer and its
absence reads as an omission.

Exit code is `0` whether or not the extraction succeeded — reporting a failure
accurately is the job, so this composes in a loop over the attention queue. It
exits nonzero only when the article does not exist, the stored page is missing
from the archive, or the database cannot be reached.

See [Add a domain rule](../how-to/add-a-domain-rule.md), which is where the
answer usually leads.

### `tome version`

Prints the build identity to stdout and exits `0`.

```
tomekeeper v0.3.0 (a1b2c3d) built 2026-08-16T23:00:03Z go1.26.5 linux/amd64
```

Version, commit, and build date are injected at link time. When they are not —
a `go build` with no flags, or `go run` — they are recovered from the Go build
info embedded from the git work tree, and a build with uncommitted changes is
reported with a `-dirty` suffix.

The version is `git describe --tags --match 'v[0-9]*'`, which makes it the same
string as the git tag and the image tag for a release (`v0.3.0`), and a description
of the distance from the last one otherwise (`v0.3.0-12-gfccf5ba`). The publish
workflow runs this command inside the image it just pushed and fails the release if
the two disagree, so a version reported by a running pod can be trusted to name the
build it came from. See [Cut a release](../how-to/cut-a-release.md).

### `tome help`

Prints usage to stdout and exits `0`. `tome -h` and `tome --help` are
equivalent.

## HTTP endpoints

Served by `tome serve`. Both respond to `GET` and `HEAD`; any other method
returns `405`. Both set `Cache-Control: no-store` and return
`application/json; charset=utf-8`.

## Web interface

Served by `tome serve`. Every route requires a session except `/login`, `/static/`,
and two documented exceptions: `/fever/`, which carries its own credential, and
`/assets/`, which additionally accepts a signature this service issued. Both are
described under [Fever API](#fever-api) below.

| Route | Page |
|---|---|
| `GET /` | Unread stream. `?category=` narrows it to one folder; present-but-empty selects the feeds with no category. |
| `GET /all` | Everything in the archive. `?category=` redirects to that category's own view, which is the same list. |
| `GET /starred` | Starred articles |
| `GET /saved` | Reading list — pages saved by hand |
| `GET /categories` | The categories the feeds are filed under |
| `GET /categories?name=` | One category's articles. Present-but-empty selects the feeds with no category. |
| `GET /articles/{id}` | The reader. Opening an article marks it read. |
| `GET /search?q=` | Search results |
| `GET /feeds` | Feed list, health, and tags. `?sort=` and `?dir=` order it, `?q=` and `?health=` filter it, `?edit=<id>` opens one subscription in the form at the top. |
| `GET /feeds/{id}` | One feed's articles |
| `GET /tags/{id}` | One tag's articles |
| `GET /attention` | Articles that did not come through cleanly |
| `GET /settings` | Palette, reading preferences, and the export download |
| `GET /domain-rules` | Extraction overrides. `?edit=<host>` loads that host's rule, or offers to create one. |
| `GET /mark-read?from=` | Asks before marking a whole list read. `from=` names the list. |
| `POST /mark-read` | Marks everything unread in one list read. Same `from=`. |
| `POST /mark-read/scrolled` | Marks named articles read after they were scrolled past. `ids=` is a comma-separated list. `409` when the preference is off. |
| `POST /articles/{id}/read` | Mark read or unread. `on=true` or `on=false`. |
| `POST /articles/{id}/star` | Star or unstar. Same form field. |
| `POST /articles/{id}/keep` | Keep permanently, or stop. Same form field. |
| `POST /articles/{id}/promote` | Show a different stored body. `body=` is its id. |
| `POST /save` | Save a page by hand. `url=`. |
| `POST /import` | Import an uploaded reading library. `library=` is the file; `report_only=true` reports without importing. |
| `GET /export` | Download the archive as a file. The one route allowed to exceed the write timeout. |
| `POST /feeds/test` | Fetch a feed URL and report what is there. Writes nothing. |
| `POST /feeds/add` | Subscribe to one feed. `url=`, optional `category=` and `title=`. |
| `POST /feeds/{id}/edit` | Change one subscription. `url=`, `title=`, `category=`, `enabled=` and `poll_every=`. An omitted `enabled` turns polling off; an empty `poll_every` means automatic. |
| `POST /feeds/{id}/unsubscribe` | Remove one subscription. `GET /feeds?unsubscribe=<id>` asks first and says what it costs. |
| `POST /domain-rules` | Save one rule. |
| `POST /domain-rules/delete` | Remove one rule. `domain=`. |
| `POST /domain-rules/reprocess` | Queue re-extraction of one domain. `domain=`. |
| `POST /feeds/import` | Subscribe to everything in an uploaded OPML file |
| `POST /feeds/refresh` | Bring every enabled feed forward to due |
| `POST /settings` | Save preferences. `palette=`, `mode=`, `mark_on_scroll=` and `poll_every=`. Posts every preference it holds, so an absent field is read as off or automatic. |
| `GET /login`, `POST /login`, `POST /logout` | Session |
| `POST /fever/` | The Fever sync API for mobile clients. Its own credential; see below. |
| `GET /assets/…` | Archived images, from `TOME_BLOB_ROOT`. A session, or a `sig=` this service issued. |
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

### `POST /feeds/test` and `POST /feeds/add` — add one subscription

The two steps of adding a feed by hand, on the **Feeds** page. They are separate
routes because they are separate decisions: testing writes nothing, and adding does
not require having tested.

**Test** is the only outbound request `tome serve` makes. It fetches the URL, and:

- If it is a feed, reports its title, how many items it carries, when the newest
  one is, and the first few item titles — enough to recognize a feed, and enough to
  notice one whose newest item is from 2019 before subscribing rather than after a
  week of empty polls.
- If it is an HTML page, follows the first RSS or Atom `<link rel="alternate">` it
  advertises and reports that feed instead, saying so. This is the common case: the
  URL somebody has to hand is the site's, not its feed's.
- If it is a page with no feed, says so and suggests what to try.
- If the reader is already subscribed, says that too. It is a reasonable question to
  use a test for.

Whatever answered becomes the URL in the form, so **Add** subscribes to the feed
rather than to the page it was found on.

An instance with no outbound HTTP client hides the Test button and, if the route is
posted to anyway, says testing is unavailable rather than failing obscurely. Adding
still works: a broken feed shows up in the feed list's health column and in
**Attention**, which is what they are for.

`Add` is `UpsertFeed`, so re-adding an existing subscription updates its title and
category and disturbs nothing about its polling — the same idempotency the OPML
import has. A URL with no scheme is assumed to be `https`, because an address bar
does not show one and that is where the URL was copied from.

The form sits **above** the feed list rather than below it. With seventy
subscriptions in the table, a form underneath is a form nobody scrolls to.

### `POST /feeds/{id}/edit` — change one subscription

The same form, with a feed loaded into it by **Edit** on its row (`?edit=<id>`). It
takes the address, the title, the category, how often the feed is checked, and
whether it is polled at all; emptying the category takes the feed out of that folder.

`poll_every` is a Go duration (`15m`, `6h`, `168h`), and empty means automatic — the
reader's general cadence from **Settings** if they have one, otherwise the learned
interval. It sets this feed's override only: the picker never comes up showing the
general preference, because opening a form and saving it would then pin every feed to
whatever that preference happened to be. A value that will not parse, is not positive,
or exceeds a year is refused with `400` and nothing else in the edit is applied.

Values below `TOME_POLL_MIN_INTERVAL` are accepted and raised to it at poll time, and
the picker leaves them out for that reason. Values above `TOME_POLL_MAX_INTERVAL` are
honored as given: the ceiling exists to stop this service polling a quiet feed for
nothing, not to stop a reader asking for less.

Five things it does to polling, none of which are visible in the row it changes:

- **A changed address discards the conditional-GET validators** and queues the feed
  for a poll now. An `ETag` the old endpoint issued means nothing to the new one, and
  sending it invites a `304` for content that has never arrived — which presents as a
  feed that looks healthy and produces nothing.
- **A changed address clears the failure count and the last error.** They belonged to
  the address that produced them, and leaving them would put a corrected feed three
  failures from being disabled for a fault that no longer exists.
- **A changed *host* clears `site_url` as well.** That column is the base relative
  entry links are resolved against, and nothing but an import writes it — so a feed
  that has moved to another host would otherwise resolve every relative link against
  a site it no longer belongs to. Cleared, the poller falls back to the feed's own
  address. Correcting a path on the same host keeps it.
- **Turning polling back on clears the failure count too**, or the next single
  failure re-crosses `TOME_FEED_FAILURE_THRESHOLD` and disables the feed again
  immediately. Turning it *off* keeps the count and the error, so the row can still
  say what went wrong.
- **A shortened cadence brings the next poll forward**, to `last_polled_at +` the new
  interval and never to `now()`. Otherwise the choice would not take effect until the
  poll it was meant to replace, up to a day later. A feed fetched two minutes ago and
  set to hourly is next checked in 58 minutes; one fetched four hours ago is due
  immediately, because it already is. A *lengthened* cadence never postpones a poll
  that was already imminent, and choosing automatic moves nothing.

Moving a feed onto an address the same reader already subscribes to is refused with
`409` and nothing is changed: two subscriptions to one feed would be
indistinguishable in the list. The message names the subscription holding that address
and says whether it is the one polling successfully, because the usual cause is an OPML
import that carried both a feed's old address and its new one — in which case the row
being edited is the spare, and what the reader wants next is **Unsubscribe** on the
form they are already looking at. Another reader's feed id is `404`, like everywhere
else.

### `POST /feeds/{id}/unsubscribe` — remove one subscription

The only route in the interface that deletes something a reader owns. Two steps:
`GET /feeds?unsubscribe=<id>` asks, and takes the subscription form's place while it
does — a destructive question underneath two save buttons is one somebody answers by
accident.

**It deletes the subscription and its `feed_items`, and no articles.** That is the
root-entity rule in one statement: a feed that turns bad must not take what it
delivered with it. Stored bodies, images on disk, tags, highlights and read state all
stay.

What it does change is reachability, and the confirmation counts it beforehand. An
article stays in the reader's lists if another of their feeds also carries it — feeds
deduplicate onto shared articles — or if they have touched it at all, because reading,
starring or saving writes an `article_state` row and that row is the second half of the
[visibility predicate](../explanation/scoping-and-access-control.md). What is left is
the articles this feed alone introduced and the reader never opened: still on disk,
still in an export, but no longer listed anywhere. Subscribing to the feed again
relinks them — articles are keyed by canonical URL, so the same rows come back with
their bodies and images.

Two things worth knowing about that residue:

- **Saved and starred articles are never affected.** A manual save writes `saved_at`,
  which is an `article_state` row, so those articles are reachable in their own right —
  and most of them were never carried by a feed at all.
- **Nothing collects the residue afterwards.** [Retention](retention.md) releases the
  bodies of articles that somebody read and then left alone; an article nobody ever
  opened has no read state and is therefore never expirable. So unreferenced articles
  persist until something removes them deliberately, and today that means SQL. This is
  a known gap rather than a design intent.

### Sorting and filtering the feed list

Every heading in the feed table is a link that sorts by that column; clicking the
column already in force reverses it. The first click takes the useful end of each
column — A first for the text columns and for **Last success**, where the point is to
find what has gone quiet, but most-first for **Unread** and worst-first for
**Health**. `aria-sort` marks the column in force, and the arrow drawn next to the
heading comes from that attribute, so what a screen reader announces and what
everybody else sees cannot disagree.

`?q=` matches a substring of a feed's title, address or category; `?health=` selects
`ok`, `failing` or `disabled`. Disabled feeds are deliberately not "failing" — a feed
that has stopped being polled is a state of its own, and asking what is failing is
asking what is going wrong now.

Both happen in Go over the rows the page has already loaded, not in SQL. The page
needs every row regardless — the failing-feed banner counts them, the category
suggestions come from the same list — and two sortable columns are not columns:
unread arrives from a separate aggregate query, and health is a rank over three
fields. So a filter costs a substring test per row against data already in memory,
and there is no query behind it to pay for. The banner still counts the whole
archive rather than the filtered view: "two feeds are failing" is a fact about the
subscriptions, and hiding it behind a search would be a way to stop being told about
a slow puncture.

The state lives in the query string, so a sorted, filtered list can be bookmarked —
and so that the forms, which render the list rather than redirecting to it, can carry
the reader's view through a save.

### `POST /articles/{id}/promote` — choose which stored copy to show

An article can hold more than one body: the page as this archive extracted it, the
copy a library was imported with, and any older extraction kept when a better one
replaced it. One is shown, and the article page lists the others below it whenever
there is a choice — with where each came from, how long it is, and its opening
words.

**This is the only way an imported body is ever replaced.** An imported body is
immutable and wins automatically, because it may be the only surviving copy of a
page that is gone, so nothing automatic may overrule it. That leaves exactly one
mechanism: somebody looks at both and says which is better.

Two consequences worth knowing:

- **Promoting is reversible.** Nothing is deleted, a demoted immutable body is still
  immutable, and it can be promoted back.
- **Promoting a mutable body puts the article back in the extraction lifecycle.**
  Re-extraction selects on the *current* body being replaceable, so an article
  showing an imported copy is excluded and the same article showing a fetched copy is
  not. That is the right behavior and it is not obvious.

**The choice is global**, like the body it chooses: the archive keeps one copy of a
page for everyone, so promoting changes what every reader sees in a way starring or
tagging never does. Correct while there is one reader; a multi-user build has to
decide whether it stays shared or becomes a per-reader preference.

### The domain rules page

Everything `tome domain-rule` does, plus the reprocess that has to follow it. The
form takes strip selectors one per line, reduces a pasted URL to its host, and
refuses a rule that would do nothing — a rule with no selector, no strips, no rate
and no JavaScript flag saves happily and changes nothing, and the only symptom is a
site that still extracts badly.

**Reprocess queues exactly what `tome reextract --target-version 0 --domain X`
queues**, through the same function, so the button and the command cannot drift. It
re-extracts from pages already stored, so it asks no site for anything and works for
articles whose sites are gone, and it never selects an imported body. Version `0`
rather than the current version is the subtlety: selection is "any version other
than this one", and a rule is data that changes between runs of one binary, so no
rule edit can bump a constant compiled into it. No body carries "0", so every
mutable body matches.

Each rule shows how many stored articles come from its host — the same host
expression the reprocess uses, so the count is what the button would act on.

**Rows in [Attention](#) link straight to the rule form for their host**, because
that is where a badly-extracted site is discovered and the fix used to begin by
leaving the browser.

Two things this page cannot do anything about, and says so rather than pretending:
a **rate** change is read once when the worker starts, so it needs a worker restart;
and **needs JavaScript** is recorded only, since headless rendering does not exist
yet.

An instance with no job queue hides the reprocess control and, if posted to anyway,
names the command that does the same thing.

**These routes are admin surface.** Rules are global — how to extract a site is a
fact about that site, identical for every reader — so they reach through the
cross-user store methods that the rest of the interface deliberately avoids. That is
correct for a single-user archive and is one of the things multi-user work has to
gate.

### `GET /export` — download the archive

The counterpart to the import upload, on the **Settings** page. Streams rather than
buffering, because an export is the one response whose size grows with the archive.

It extends its own write deadline to ten minutes rather than raising the server's
30-second timeout for everything: an export runs at roughly three seconds per 385
articles, so the default would start failing somewhere past eight thousand — a
number an archive reaches quietly, years in, with no explanation attached.

Streaming costs one thing worth knowing: the status is sent before the work is done,
so a failure partway through cannot be reported as an error and the download simply
ends early. That is survivable because the file is JSON and the importer refuses a
truncated one outright, naming it as ending before its last record. A cut-off
download is caught on the way back in rather than restored as half an archive.

### `POST /import` — import a library through the browser

The upload form on the **Saved** page, doing what `tome import` does. Same two
passes over the file, so a corrupt or truncated export writes nothing; the file is
spooled by the multipart parser and read twice from disk rather than held in memory
or uploaded twice.

| Field | Meaning |
|---|---|
| `library` | The export file. The format is detected from its first bytes, not its name. |
| `report_only` | Present means report what the file holds and import nothing. |

Uploads are capped at 128MB. A library past that — or one large enough that the
import exceeds the server's 30-second write timeout — belongs on the command line,
where neither limit applies. If a timeout does interrupt one, re-uploading is safe:
every write is idempotent and the second run continues from where the first
stopped.

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

### `POST /mark-read/scrolled` — marking read on the way past

The only place in this interface where reading state changes without anybody
pressing anything, which is why it is off until a reader turns it on in
[Settings](#web-interface) and why the rules around it are narrow.

`ids=` is a comma-separated list of article ids the page reports having gone past.
The response is the affected rows' controls as htmx out-of-band fragments — the same
partial a click returns — so exactly the rows that changed are redrawn and nothing
else is. Nothing marked means an empty response.

| Condition | Result |
|---|---|
| The preference is off | `409`. The check is here rather than only in the page, so turning it off takes effect on tabs that are already open. |
| An id is not a positive number, or the list is malformed | `400`, and nothing is marked. All or nothing: only a script posts here, and the symptom of a script bug should not be a partial mark. |
| More than 200 ids | `400`. A page is 50 rows, so this is only reached by something other than the page — and refusing beats marking the first 200 and dropping the rest silently. |
| An id the reader cannot see | Absent from the response, and nothing is written. The same nothing a nonexistent id gets. |
| Starred or saved | Never marked. Both mean somebody said the article matters, and scrolling past it is not a decision to be finished with it. |
| Kept | Marked. Keeping protects the stored body from retention, which says nothing about whether it has been read. |
| Already read | Not written, so it keeps its original `read_at` — see the bulk mark above for why that matters. |

Which lists ask for it is narrower than which may be marked in bulk: **the unread
lists only**, meaning Unread and Unread narrowed to a category. Everything, a
category, a feed and a tag are where a reader goes to *find* an article, and
scrolling through them looking for one must not mark the archive read on the way.

### Navigation between lists and articles

An article link carries the list it was opened from, as `?from=<token>`:

| Token | List |
|---|---|
| `unread`, `all`, `starred`, `saved` | The corresponding stream |
| `feed:{id}` | One feed |
| `tag:{id}` | One tag |
| `category:{name}` | One category, `{name}` verbatim |
| `unread-category:{name}` | The unread list narrowed to one category |
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

Every token has the variable part last, which is why the narrowed unread list is
`unread-category:{name}` and not `unread:category:{name}`. A category is a folder name
from somebody else's reader and may contain a colon — "Cooking: BBQ" is a plausible
folder — so the argument has to be taken whole rather than split.

### Narrowing a list to one category

**Unread** and **Everything** carry a row of category links above the list, and so do
the category views themselves, so a reader can move sideways between folders. Where
each link goes depends on which list is asking:

- From **Unread**, `?category=Comics` gives the unread list narrowed to that folder —
  the one combination that has no other home. It is a list in its own right, so it
  pages, it has previous/next, and its **Mark as read** covers exactly those articles:
  clearing one folder without touching the rest.
- From **Everything**, the link points at `/categories?name=Comics`, which *is*
  everything in that folder. Rendering the same articles at a second address would
  leave a Next button computed from a different definition than the list it belongs
  to, which is the failure the one-table-of-list-definitions rule in `streams.go`
  exists to prevent.

A category view is deliberately not unread-only, which is where this differs from
Miniflux and FreshRSS — both default a folder to its unread items. A category here is
the way back to a folder, and every stream is ordered
`COALESCE(published_at, first_seen_at) DESC`, so anything new is already at the top
without the list hiding what it holds.

Links rather than a dropdown, for two reasons. One click instead of two, with no
JavaScript needed to make a `<select>` navigate. And the nameless category — feeds an
OPML file listed outside any folder — has to be selectable *and* distinguishable from
"do not filter at all", which one form value cannot express and two hrefs can.

The control is not offered on the lists where it would be meaningless: a feed is
already inside one category, a tag deliberately crosses them, search has its own
query, and the reading list holds pages that came from no feed and therefore have no
category. It is also not drawn when there is only one bucket to choose from.

The names come from `ListCategoryNames`, which reads `feeds` alone. The counts on the
Categories index need an aggregate over `feed_items`, `articles` and `article_state` —
measured at 2.7ms against a 1,900-article archive versus 0.15ms for the names, and
only the first figure grows with the archive. This control is on the most-requested
page in the interface, so it takes the cheap query and leaves the counts to the page
that shows them. It is also skipped entirely on the htmx requests that load further
pages, which want rows rather than a document.

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
| `j` past an entry | Also marks it read, when marking on scroll is on |
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

### Fever API

`POST /fever/` is the sync protocol mobile clients speak, and `POST /fever` without the
trailing slash is registered alongside it so that a client omitting it is not answered
with a redirect it may not repeat the body for.

It is outside `requireUser` because it authenticates differently, not because it is
open: the credential is `api_key`, MD5 of `username:password`, held in
`users.api_key` and written whenever the password is set. A request without a valid one
gets `{"api_version": 3, "auth": 0}` and nothing else, with **HTTP 200** — this
protocol reports its result in the body.

Every argument, object, limit and deviation is in
[the Fever API reference](fever-api.md);
[How to connect a mobile client](../how-to/connect-a-mobile-client.md) is the setup.

#### Signed asset URLs

`GET /assets/…` accepts a session — as it always has — **or** a signature:

```
/assets/sha256/a1/b2/….avif?sig=<expiry>.<base64url HMAC-SHA256>
```

This exists for one caller. A Fever client renders an article body in its own view
with no session cookie, and an `<img>` tag cannot carry a POSTed credential, so a
body's image references have to authorize themselves. The signature covers the path
and the expiry together, so it cannot be moved to another image or extended.

The key is derived from `TOME_SESSION_KEY` by HKDF with the label
`tomekeeper asset url v1` — a different label from the session cipher's, so the two
keys are independent. Rotating that secret invalidates outstanding image URLs along
with every session. URLs last 30 days.

One query parameter rather than two, deliberately: these URLs are written into an HTML
attribute, where an `&` between parameters serializes as `&amp;`, and whether the image
then loads would depend on the client's HTML parser rather than on this service.

### Response headers

HTML responses carry
`Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self';
img-src 'self' data:; font-src 'self'; connect-src 'self'; manifest-src 'self';
form-action 'self'; base-uri 'none'; frame-ancestors 'none'`.

Nothing is `unsafe-inline` and nothing is third-party. That is affordable precisely
because the script and the fonts are vendored and the images are localized — a page
needing a CDN could not have a policy this tight.

Two directives are here because `default-src 'none'` blocks something whose absence
does not report itself:

- `manifest-src`, without which "add to home screen" simply offers a generic icon
  and the wrong name.
- `font-src`, without which an `@font-face` pointing at this origin fails silently
  and every page renders in the fallback stack — indistinguishable from a reader
  who does not have the font installed.

### Static asset caching

| Path | `Cache-Control` |
|---|---|
| `/static/…` | `public, max-age=300` |
| `/static/vendor/…` | `public, max-age=31536000, immutable` |
| `/assets/…` | `public, max-age=31536000, immutable` |

The split is the naming rule: everything under `vendor/` carries its version in its
filename, so those bytes never change and an upgrade is a different URL. The
stylesheet does not, so it keeps a short cache — a year-long one would strand a
changed stylesheet in every browser after an upgrade.

It matters most for the fonts, which are 320KB: on the stylesheet's policy a reader
would revalidate them every five minutes forever, which would make shipping fonts
slower than the system stacks they replace.

Archived images under `/assets/…` are content-addressed, so there an immutable cache
is a fact about the bytes rather than a promise about naming discipline.

`.woff2` responses are typed `font/woff2` explicitly rather than by extension
lookup. Go's `mime` package has no builtin entry for it and resolves the extension
through `/etc/mime.types`, which the distroless runtime image does not have.

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
  It mattered most when deployments followed a moving tag, where a pod restart
  could pull a binary newer than the schema with nobody having erred; deployments
  pin a release now, and the check is still the thing that makes an upgrade run in
  the wrong order visible rather than mysterious.
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
| `tome reindex` — rebuild the search index | Not currently needed: the search index is a generated column PostgreSQL maintains itself. |

Invoking either exits `2` as an unknown subcommand. They are named here so that
the absence is a documented gap rather than something you conclude from a failed
command.
