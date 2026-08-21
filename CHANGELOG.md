# Changelog

Notable changes, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Every release is a git tag `vX.Y.Z`, and the container image published for it
carries **the same string**: `ghcr.io/runlevel-six/tomekeeper:v0.12.0` is the tag
`v0.12.0`, and `tome version` inside it says `v0.12.0`. One identifier, everywhere,
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

### Fixed

- **The unread count no longer drifts while you read.** It is shown in four places —
  the document title, the nav badge, the tab bar badge and the installed app's icon —
  and every one of them was rendered at page load and then left alone, so marking
  articles read by scrolling silently made all four wrong until the next navigation.
  The response now carries the fresh total and the page applies it everywhere.

  Fixing only the app icon was the obvious small change and would have been worse:
  two numbers on one screen that disagree read as a broken count, where four equally
  stale ones read as a page that needs reloading.

  The count also re-asserts itself on the app icon when the app returns to the
  foreground, since the platform may have cleared it while away.

## [v0.12.0] — 2026-08-21

**Adds a migration.**

### Added

- **Categories can be created, renamed and deleted from the interface.** A category
  used to be free text on a subscription, existing exactly as long as some feed
  claimed it — so there was no object to create, an empty folder was impossible, and
  renaming one meant rewriting every feed in it.

  **Deleting asks what happens to the feeds** — leave them filed under nothing, move
  them to another category, or unsubscribe them — and **no answer touches an
  article.** Nothing in this project deletes one, and an article has no category of
  its own to lose: it is derived through `feed_items` to the feed that carried it, so
  refiling a feed moves everything it ever brought in. The unsubscribe option says
  plainly that the archive is kept *and* that anything never opened stops being
  listed, which is the same consequence unsubscribing one feed has always had.

  The nameless bucket is deliberately not manageable. It is the absence of a category
  rather than one named for absence, so there is nothing there to rename or delete.

- **The feed form offers the categories that exist, and an explicit "no category".**
  It was a free-text field with suggestions, on the grounds that a category existed
  only because some feed claimed one — there is a list now. The old arrangement also
  had a real gap: filing a feed under nothing meant *emptying* the field, which
  worked and which nobody discovered. A companion field still names a new category,
  and a typed name wins over the picker, which the form says out loud.

### Fixed

- **Renaming a category no longer reshuffles a synced client's folders.** The Fever
  group id was a hash of the category's *name*, because the protocol requires an id
  and there was no row to take one from — so a rename made the old folder vanish from
  a client and a new one appear holding the same feeds. Clients cache folder
  membership against those ids. The id now belongs to the category, and 57 lines of
  hashing and collision-rehandling are gone.

### Migrations

- **00013** adds `categories` and `feeds.category_id`, backfilling from the old
  column. `feeds.category` is deliberately **not** dropped: `internal/db`'s schema
  guard treats a database newer than the binary as safe on the stated grounds that
  "the old binary's queries still work against a superset schema", which is true only
  while migrations are additive. Dropping it would leave an older binary passing the
  guard and then failing on every query naming it — the outage that guard exists to
  prevent, from the other direction. It is frozen at the backfill and droppable once
  no deployable binary reads it.

## [v0.11.0] — 2026-08-21

### Added

- **Pull down from the top to reload**, on any page. It follows the reload control,
  which is the same thing `r` does — installed on a phone there is nothing else that
  can ask for newer articles: no address bar, no reload button, and no
  pull-to-refresh of the platform's own in a standalone window. The comment on that
  control has said so since it was written.

  It reloads the page and does **not** poll the feeds. That control is on the Feeds
  page and labeled, because it costs every subscribed site a request, and a gesture
  this easy to perform must not be the one that spends them.

  Where the browser has its own pull-to-refresh — Android's does — it is now
  suppressed, so the two cannot both fire and reload twice. In a standalone window
  there was never one to suppress, which is the case this exists for.

  The header follows the finger and the reload glyph turns as the pull completes;
  past the threshold the control highlights, the same way the mark-read control does
  at the end of a list. Which gesture fires is decided by direction while the drag is
  happening, so a long diagonal cannot satisfy both the swipe and the refresh and
  navigate twice.

## [v0.10.1] — 2026-08-21

### Fixed

- **A text size no longer changes the layout width.** Every step now gets the column
  the largest step produced, which is what it was asked for after use. The widths are
  pinned to that step's value and divided back out by whatever scale is in force, so a
  column follows the reader's *browser* font size but not their chosen text size —
  changing the text size changes the text and nothing else.

  This reverses the reasoning the preference shipped with, which grew the column with
  the type to hold roughly 68 characters per line. It no longer holds: the largest step
  gives nearer 71 characters and the smallest nearer 97. A stable layout was preferred
  to a stable measure, deliberately, and it is written down so nobody restores the old
  behavior thinking it was an oversight.

  The sign-in box still hugs its content — nobody is signed in there, so there is no
  chosen size to honor. Breakpoints needed no change: `rem` inside a media query
  resolves against the browser's initial font size rather than the root element's, so a
  text size cannot move one.

## [v0.10.0] — 2026-08-21

**Adds a migration.**

### Added

- **A text size preference in Settings**, four named steps. Like the palette it is a
  column on `users` rendered into the first paint rather than a cookie read by
  script — and for a sharper reason: a size applied after layout reflows the whole
  page under somebody who has started reading it.

  It scales the **root** font size as a percentage, so one number moves every view
  and it multiplies whatever font size the browser is already set to instead of
  replacing it. **Layout widths stay put** — they are pinned to the widest step's
  value, so changing the text size changes the text and nothing else. Archived
  standalone pages keep their own typography, as they must: they have to open with
  none of this running.

### Fixed

- **The interface overrode the browser's own font-size setting.** `body` was pinned
  at `16px`, so everything inheriting from it ignored a reader who had already asked
  their browser for larger text. It is `1rem` now. This changes rendering for anyone
  whose browser font size was not the default — in their favour.
- **The article had four supporting type sizes pretending to be distinct.** The
  byline at 12.8px, the outbound link at 13.6, the image notice at 13.6 and captions
  at 14.2 — 1.4px apart on a phone, which reads as one size with rounding errors —
  and none of them moved when the body did. Now one supporting tier derived from the
  body, so the article holds its proportions at every width and every size step.
  Three levels rather than four is deliberate: four do not fit in the range a phone
  has available.

## [v0.9.0] — 2026-08-21

### Fixed

- **The README generated an unusable Postgres password about 40% of the time.** It
  still said `openssl rand -base64 24`, which was corrected in
  [Install on Kubernetes](docs/how-to/install-kubernetes.md) and nowhere else:
  base64 contains `/`, and a `/` ends the authority section of
  `postgres://tome:PASSWORD@host/db`. 32 base64 characters carry one with
  probability 1 − (63/64)³². The other two secrets on that line are not put in a
  URL and stay base64. `scripts/check-release.sh` reads `deploy/` and
  `compose.yaml` only, so nothing was ever going to catch this but reading it.

### Added

- **Swipe left-to-right on an article to go back to the list.** Follows the same
  link `u` does, so there is one way back rather than a second implementation of
  one. Left-to-right because that is the direction the whole device uses for this;
  right-to-left deliberately does nothing, since previous and next are buttons and
  the article nav sits at both ends of every article.

  It gives up if the drag turns into a scroll — judged on the way, not at the end,
  because reading is mostly vertical scrolling — and a drag that begins inside
  something scrollable sideways is left alone, so the wide code blocks and tables in
  archived bodies stay usable. The article follows the finger while you drag, which
  is the only feedback available: the nav it would otherwise highlight is off screen
  from the middle of an article.

- **`task test:js`** — the touch gestures, run against a stub DOM by node and
  nothing else. No package.json, no framework, no build step. It earned its place
  immediately: two of the six guards it now covers turned out to be untested by the
  sequences written first, which the neuters found and hand testing could not.

## [v0.8.0] — 2026-08-21

### Fixed

- **`scripts/check-release.sh` could silently stop checking an overlay.** It read
  the version pin through a two-line window after the image name, so a comment
  written in between pushed the pin out of view — and an unparsed override was
  *skipped*, not failed. A half-finished version bump would have passed, with no
  trace but a count in a pass message dropping from 3 to 2. It now reads the whole
  image entry, and recognizes a `digest:` pin as the other legitimate way to name
  an image rather than as an absence.

### Added

- **A list can be marked read from its end, and on a touch screen by pulling past
  the bottom.** The control beside the heading is the wrong one to reach for after
  reading forty pages — it is forty pages up — and marking read as you scroll can
  never reach the last screenful, because those rows never leave over the top edge.
  So the bottom of a list was exactly where a reader had finished and had nothing to
  say it with.

  Both controls are the same request and the same two-step confirmation; there is no
  second path to the write. The pull gesture follows the link rather than posting,
  so an accidental pull costs a page you navigate away from. With no JavaScript the
  control is still there to tap.

  The end-of-list control is drawn by whichever render reaches the end — the
  document for a short list, the last appended fragment for a long one — because
  rows are appended as they are revealed and nothing fixed in the document is ever
  at the bottom. Its unread count is counted on that final page only, not once per
  page on the way down.

## [v0.7.0] — 2026-08-21

Two corrections to v0.6.0, both found by running it against a real archive rather
than by reading it. **Adds a migration.**

### Extraction version 7

**Version 6 reached one of the ten strips it was written for.** It matched a short
slug against an image's whole file name, which was generalized from a single
example: the site in question names its files `171-err.png`, and only one strip
happened to be `10x.png`. The signal it missed was the folder —
`/2020/err/171-err.png` — so a short slug is now matched against any complete
**path component**, which is what "strong enough a claim to trust" actually meant.

Measured over every article in a real archive whose slug is under four characters,
which is the only set this branch can affect: **7 gained their strip, nothing else
changed.**

Run `tome reextract` once after upgrading.

### Fixed

- **A reprocess that produces nothing no longer files an article that already has a
  body as a failure.** A version bump runs the current ladder over every stored
  page, and a body produced by older behavior may simply not extract again — the
  reader still has the article and nobody can act on it. The version 6 catch-up run
  put eight such articles into the attention queue in one pass, hours after
  `00010` cleared out the last set. Same emptiness as that fix, arriving from the
  other direction.
- `store.Article` now carries `extract_attempt_version`, which until now was
  written and read only by the query that selects articles to reprocess. A column
  nothing reads is a shape of bug this archive has already found twice.

### Migrations

- **00011** repeats `00010`'s correction for the rows the version 6 run left
  behind. Same conditions, same reasoning: a stored page proves the fetch worked,
  and an imported body over a genuinely failed fetch keeps its failure.

## [v0.6.0] — 2026-08-21

Draining the extraction tail: seven domain rules, one rung that could not see short
slugs, and a queue that would not empty when you fixed it. **Adds a migration**, so
upgrading is the migration Job and then the apply.

The rules themselves are data rather than code — they live in the `domain_rules`
table and are set with `tome domain-rule set`, so nothing here ships them. What
shipped is the two things the rules could not fix by themselves.

### Extraction version 6

A slug too short to match as a substring is now matched against an image's whole
file name rather than discarded. The four-character floor was standing in for "is
this claim strong enough to trust", and an exact file name is a strong claim at any
length — so the rung written for image-only pages could not reach a strip at
`/2025/10x` named `10x.png`. Ten of them on a real archive, on a site where the
file name is always the slug.

Run `tome reextract` once after upgrading.

### Fixed

- **An article rescued from a page already on disk no longer stays in the attention
  queue forever.** An extraction that produces nothing is recorded as a *fetch*
  failure, and nothing ever took that back once a domain rule found the body. On a
  real archive, 409 articles with a good current body were still listed as failed,
  314 of them extracted by a rule. A queue that does not empty when you fix things
  is a queue people stop reading. The failure is now retired whenever an extraction
  becomes an article's current body and a stored page proves the fetch itself
  worked; an imported body whose page fetch genuinely failed keeps its failure,
  because the archive really is missing that page. Migration `00010` does the same
  correction once for everything extracted before this.
- **`tome explain` no longer reports a rule that matched something too short as a
  rule that matched nothing.** Those want opposite remedies — the page's markup
  versus the length floor — and the explanation sent every rejection to the first.

## [v0.5.0] — 2026-08-21

One fix, and it is the one worth upgrading for: extraction improvements can finally reach
the articles they were written for. **Adds a migration**, so upgrading is the migration Job
and then an apply — and then one `tome reextract` to collect the backlog.

### Fixed

- **`tome reextract` could not reach an article that produced no body**, which is the most
  expensive bug found in this project so far. Candidates were selected by comparing the
  extractor version that produced an article's *body* — so an article with no body was
  never a candidate, and **every extraction improvement since the second milestone
  silently skipped exactly the articles improvements are written for.**

  Measured when it was found: **343 articles with a stored page and no body, 280 of them
  webcomics from a single host** — and the image rung added three versions earlier
  archives them today. Their pages had been on disk since the first poll with nothing able
  to point at them.

  Failures now record the version that attempted them, so the same "other than this
  version" comparison works for both outcomes. **A one-off `tome reextract` after taking
  this release picks up the backlog**; it touches no origin server, as ever.

### Migrations

- **00009** adds `articles.extract_attempt_version`. Nullable, and compared with
  `IS DISTINCT FROM` rather than `<>` — `NULL <> '5'` is NULL, not true, so a plain
  inequality would have excluded every article the column was added to reach. That is the
  same shape of silent-exclusion bug it fixes, which is why it is called out here.

## [v0.4.0] — 2026-08-21

Headless rendering explains itself. Everything here came out of walking through what a
user actually sees when they add a site that turns out to need a browser — and finding
that the answer was mostly "nothing". **Adds a migration**, so upgrading is the migration
Job and then an apply.

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

[Unreleased]: https://github.com/runlevel-six/tomekeeper/compare/v0.12.0...HEAD
[v0.12.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.11.0...v0.12.0
[v0.11.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.10.1...v0.11.0
[v0.10.1]: https://github.com/runlevel-six/tomekeeper/compare/v0.10.0...v0.10.1
[v0.10.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.9.0...v0.10.0
[v0.9.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.8.0...v0.9.0
[v0.8.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.7.0...v0.8.0
[v0.7.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.6.0...v0.7.0
[v0.6.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.5.0...v0.6.0
[v0.5.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.4.0...v0.5.0
[v0.4.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.3.0...v0.4.0
[v0.3.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.2.0...v0.3.0
[v0.2.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/runlevel-six/tomekeeper/releases/tag/v0.1.0
