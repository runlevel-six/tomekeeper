# Data model

The schema is defined by the migrations in `internal/db/migrations/`, which are
embedded in the binary and applied by [`tome migrate`](cli.md#tome-migrate).
This page describes what those migrations create. Where the two disagree, the
migrations are correct.

Migrations are append-only. A migration that has run anywhere is never edited;
corrections are new files.

**Requires PostgreSQL 16 or later.**

## Scoping

Three groups of tables, with different ownership rules.

| Group | Tables | Scope |
|---|---|---|
| User-owned | `feeds`, `feed_items`, `article_state`, `tags`, `highlights`, `import_records` | One user. Every query carries a `user_id`. |
| Shared pool | `articles`, `article_content`, `assets`, `article_assets` | Global. One archived copy serves every user. |
| Global admin | `domain_rules` | Global. Extraction rules are technical, not personal. |

The shared pool is why two people subscribed to the same site get one archived
copy and one set of images. It is also why user-facing queries must reach
articles by joining through `feed_items` or `article_state` rather than reading
`articles` directly — see [why articles are the root
entity](../explanation/why-articles-are-the-root-entity.md).

## Tables

### `users`

Readers, and the two things authentication needs to know about each: what they
may change, and whether the credential in their browser is still good. The first
account is created by `tome migrate` from `TOME_USERNAME`.

| Column | Type | Notes |
|---|---|---|
| `id` | `bigserial` | Primary key. The seed user is always `1`. |
| `username` | `text` | Unique. |
| `password_hash` | `text` | An argon2id hash in PHC string form, written by `tome migrate` from `TOME_PASSWORD`. Empty means the web interface cannot be signed into, which is what a first run with no password set produces. The parameters live inside each hash, so raising the cost later is an upgrade rather than a lockout. |
| `api_key` | `text` | Unique, nullable. MD5 of `username:password`, the credential [the Fever API](fever-api.md) authenticates against. It cannot be derived from `password_hash`, so it is written while the cleartext is in hand — which is why the column predated the API that now reads it. Null until a password is set, and rewritten on every change, so changing a password disconnects every mobile client. |
| `retain_after_read` | `interval` | How long this reader keeps a read article listed. Nullable, meaning "follow `TOME_RETAIN_AFTER_READ`"; zero is a real value meaning keep everything. It expires *this reader's view* — their state, and their own body if they have one. It cannot reclaim the shared page or the shared images, because another reader may still hold them; that is [`tome prune`](cli.md#tome-prune), which operates on what nothing references at all. |
| `role` | `text` | `admin` or `reader`, constrained by the database. Not about who may *read* what — reading is scoped per user by construction — but about who may change what everyone shares: domain rules, retention, the archive-wide audit, and other accounts. The account `tome migrate` seeds is an admin, since it is the operator and there is nobody to grant it anything. |
| `session_epoch` | `bigint` | Bumped to invalidate every outstanding session for this reader at once. The value is sealed into the session cookie when it is issued and compared on every request, so a password change, an explicit sign-out-everywhere, or deleting the account all take effect immediately rather than when a cookie happens to expire. It buys revocation per reader, not per device — the trade taken instead of a sessions table, which the session interface still leaves available. |
| `theme` | `text` | The reader's palette and light/dark preference, as one value such as `plum-dark` or `auto`. See [Themes](themes.md). |
| `mark_read_on_scroll` | `boolean` | Whether the unread lists mark articles read as they are scrolled past. `false` unless the reader turned it on; automatic state changes are opted into, never inherited from an upgrade. |
| `default_poll_interval` | `interval` | How often the reader wants their feeds checked. Nullable, and null is the default and a real value: it means the poller decides per feed. A feed with a `poll_interval_override` does not consult this. |
| `created_at` | `timestamptz` | |

**Bodies and rules have an owner.** `article_content.user_id` and
`domain_rules.user_id` are nullable, and `NULL` is the household's — the extraction
and the rules everybody gets until their own diverge. A reader gets a row of their
own only when it does, so a household of readers who never write a rule stores one
body per article, exactly as a single-user archive does.

The line that draws: **the household owns what costs bandwidth or disk** — the
fetched page and the content-addressed images, which are 63% of this archive and
are shared however many readers hold them. **The reader owns what is derived from
it** — the body, the rules that produced it, and which stored copy they see.

One current body per article *per owner*, held by a unique index on
`(article_id, COALESCE(user_id, 0)) WHERE is_current`. The `COALESCE` matters:
`NULL` is not distinct from `NULL` in a unique index, so without it an article could
accumulate any number of current household bodies.

### `password_setup_links`

Single-use links for setting a password without an administrator learning it. One
table serves both cases that need it — a new account with no password yet, and a
reader who has forgotten theirs — because both end in the same act.

| Column | Type | Notes |
|---|---|---|
| `id` | `bigserial` | Primary key. |
| `user_id` | `bigint` | References `users`. Cascades on delete, so removing an account takes its outstanding links with it. The account always exists first: a link that created one on redemption would mean an unauthenticated request choosing a username. |
| `token_sha256` | `text` | Unique. SHA-256 of the token, **never the token**. It is a credential for setting a credential, so a copy of this table must not yield anything usable. SHA-256 rather than argon2id because the input is 256 bits from `crypto/rand` — there is no dictionary to slow down, and a redemption should not cost a KDF. |
| `created_at` | `timestamptz` | |
| `expires_at` | `timestamptz` | A week out. Days rather than hours because the link is handed over out of band — read out, messaged, written down — and there is no mail here to make it instant. |
| `used_at` | `timestamptz` | Set on redemption. The row is kept rather than deleted so a spent link reports "no longer usable" instead of "never existed". Issuing a new link marks any earlier unused one spent in the same transaction. |

Redemption is one transaction: the row is claimed by an `UPDATE` that only matches
an unused, unexpired link, and the password is written only if that matched. Two
people racing one link therefore produce one password change and one refusal.

### `feeds`

A user's subscriptions, and everything the poller needs to know about each.

| Column | Type | Notes |
|---|---|---|
| `id` | `bigserial` | Primary key. |
| `user_id` | `bigint` | References `users`. Cascades on delete. |
| `feed_url` | `text` | The URL polled. Unique per user. Stored exactly as imported — **not** canonicalized, because a query parameter on a feed endpoint may select which feed is served. |
| `site_url` | `text` | The human-readable site. Used as the base for resolving relative entry links. Written by an import and cleared when an edit moves the feed to another host, since the site it named is no longer this feed's. |
| `title` | `text` | Never empty; falls back to the feed URL. |
| `category` | `text` | From the OPML folder. Nested folders are joined with `/`. Nullable, and a null and an empty string mean the same thing — a feed the export listed outside any folder. This column *is* the category list: there is no `categories` table, so a category exists as long as some feed claims one, and re-importing a rearranged OPML rearranges them. |
| `etag`, `last_modified` | `text` | Conditional-GET validators from the last successful poll. |
| `poll_interval` | `interval` | The interval in force: learned from the feed's behavior, or the reader's cadence once one is set. Rewritten by every poll, which is why a reader's choice is stored separately — a preference kept here would not survive being polled. |
| `poll_interval_override` | `interval` | How often the reader wants *this* feed checked, overriding `users.default_poll_interval`. Nullable; null means neither is set on the feed and the general preference — or the adaptive interval — applies. |
| `next_poll_at` | `timestamptz` | When this feed becomes due. Defaults to `now()`, so a new subscription is polled promptly. |
| `last_polled_at`, `last_success_at` | `timestamptz` | A feed with a recent poll but a stale success is failing. |
| `consecutive_failures` | `int` | Reset to 0 by any success, including a 304. |
| `last_error` | `text` | Preserved after disabling, so the failure can be diagnosed. |
| `disabled` | `boolean` | Set automatically at `TOME_FEED_FAILURE_THRESHOLD`. |

Indexes: `UNIQUE (user_id, feed_url)`; `feeds_due_idx` on `(next_poll_at) WHERE
NOT disabled`, which is the scheduler's hot path.

`next_poll_at` is also the whole mechanism behind **Check all feeds now** in the
web interface: it sets this column to `now()` for the reader's enabled feeds and
lets the scheduler pick them up. Nothing else about a feed changes, which is why
pressing the button repeatedly is safe — and why it declines to move a feed polled
in the last five minutes.

Deleting a feed — **Unsubscribe** on its edit form — cascades to `feed_items` and stops
there. No article, body, asset or state row is touched: `feed_items.article_id` is a
plain reference, not an ownership edge. An article that no surviving `feed_items` row
and no `article_state` row points at is unreachable through the interface but still
present, and re-subscribing relinks it by canonical URL.

`feed_url`, `title`, `category`, `disabled` and `poll_interval_override` are the five
columns the **Edit** control on the feeds page writes. It is not an upsert: an import
preserves what it is not given, so re-importing an OPML file cannot unfile every feed,
whereas emptying the category in the form means exactly that. Changing `feed_url` also
clears the validators and the failure state and brings `next_poll_at` forward — see
[CLI](cli.md#post-feedsidedit--change-one-subscription) for why each of those has to
happen.

An edit that shortens the cadence moves `next_poll_at` too, but only as far as
`last_polled_at + poll_interval_override`, never to `now()`. The distinction is the
difference between a cadence and a refresh: choosing hourly on a feed fetched two
minutes ago means the next check is in 58 minutes, and a feed last fetched four hours
ago is due immediately because it already is. Setting the general preference on
Settings does the same across the reader's feeds, skipping those with an override of
their own and those that are disabled, and only where it brings a poll forward —
choosing a longer cadence never postpones a poll that is already imminent.

### `articles`

The root entity. A feed item, a manual save, and an imported entry are all
*references to* an article.

| Column | Type | Notes |
|---|---|---|
| `id` | `bigserial` | Primary key. |
| `host` | `text` | Generated and stored from `url_canonical`, indexed. Every query that needed a host recomputed it with `split_part` over an unindexed scan; a rule change has to find the articles it affects, and this is the difference between an index lookup and a full scan. |
| `url_canonical` | `text` | Unique. Produced by `internal/urlcanon`. This constraint is what makes deduplication work. |
| `url_original` | `text` | As first seen, tracking parameters intact. |
| `title`, `author`, `site_name`, `language` | `text` | Nullable. Filled from whichever reference first supplied each; existing values are never overwritten. |
| `published_at` | `timestamptz` | |
| `first_seen_at` | `timestamptz` | |
| `raw_blob_sha` | `text` | SHA-256 of the original fetched bytes. Null for imports with no original. |
| `raw_blob_path` | `text` | Where the gzipped page is in the blob store. Stored rather than derived: a title can change on a later poll, and a derived path would then point at nothing. |
| `raw_fetched_at` | `timestamptz` | |
| `fetch_status` | `text` | `pending`, `ok`, `failed`, `skipped`. Constrained by `CHECK`. Polling alone leaves everything `pending`. |
| `fetch_error` | `text` | |
| `extract_attempt_version` | `text` | Nullable. The extractor version of the last extraction *attempt*, whatever came of it. A body records the version that produced it; a failure had nowhere to record one, so reprocessing could not tell a failure under an old extractor from one under the current extractor and excluded every failure. Compared with `IS DISTINCT FROM` when selecting candidates, because NULL has to count as out of date. |
| `page_visible_chars` | `int` | Nullable. Visible text length of the stored page, measured at extraction. **NULL means not measured** — every article extracted before this column existed — which the interface reports as "not measured" rather than as zero. It is the number that tells a JavaScript shell (a few hundred characters) from a page whose structure defeated the extractors (thousands), and those want opposite remedies. Read by the failed-fetch queue and `tome explain`. |
| `browser_rendered` | `boolean` | `NOT NULL DEFAULT false`. The stored page is the DOM a headless browser produced rather than the bytes the server sent. Recorded at fetch time rather than inferred from the domain rules in force when somebody asks, because rules change and an article fetched plainly in March would otherwise start claiming it had been rendered the moment its domain was flagged in September. Read by `tome explain`. |
| `assets_status` | `text` | `pending`, `ok`, `partial`, `none`. Constrained by `CHECK`. `partial` means at least one image could not be localized. `pending` is strictly transient: an article whose pipeline ended without a body is set to `none` when the failure is recorded, because the asset scheduler joins the current content row and could never reach it otherwise. |
| `content_expired_at` | `timestamptz` | When [retention](retention.md) released this article's body and images. Non-null means the article is still here and what it said is not — the row, its URL and its metadata survive, so the link still works and re-fetching re-archives it. Null under the default configuration, which expires nothing. |

Note which of these are *not* per-reader. Whether a page was fetched, and whether
its images were localized, is a fact about the page rather than about anyone
reading it — so the failed-fetch queue is built from `fetch_status`, never from
per-reader state. `assets_status` is the trap: it looks like progress and is not a
readable/unreadable signal, and an early version of that queue keyed on it and
therefore hid 346 articles that had already settled.

**There is deliberately no `origin` or `immutable` column here.** This row is
shared by every user, so "how did this arrive" has no single answer.
Per-reference provenance lives in `feed_items`, `import_records`, and
`article_state.saved_at`; per-body provenance lives in
`article_content.content_origin`.

`articles_awaiting_extraction_idx` on
`(first_seen_at) WHERE fetch_status = 'ok' AND raw_blob_sha IS NOT NULL`, the
extract worker's hot path.

### `article_content`

Extraction is a derived, versioned view over the raw fetch, not the article
itself. Extraction quality only improves, so bodies are regenerable.

| Column | Type | Notes |
|---|---|---|
| `id` | `bigserial` | Primary key. |
| `article_id` | `bigint` | References `articles`, cascading. |
| `extractor_name`, `extractor_version` | `text` | `tome reextract` selects on these. |
| `content_origin` | `text` | `fetched`, `feed_body`, `import:wallabag`, … Provenance of *this body*. |
| `ruleset_key` | `text` | Identifies the extraction rules that produced this body, hashed from the content and strip selectors alone. Beside `extractor_version` it answers "is this body stale", which is what lets a sweep re-derive outstanding re-extraction — and that matters because the server and the worker are separate Deployments, so a rule can be changed while the worker is down and eagerly queued work simply lost. Empty means no rule applied, which is distinct from a rule that selects nothing. Changing a *fetch* setting does not change it, so tweaking a rate limit reprocesses nothing. |
| `immutable` | `boolean` | True for imported bodies, which may be the only surviving copy of a dead URL. The re-extract pass skips them and they are never overwritten. |
| `content_html` | `text` | Sanitized, with image sources rewritten to blob paths. |
| `content_text` | `text` | Plain text, for search. |
| `word_count` | `int` | |
| `is_current` | `boolean` | At most one current row per article, enforced by a partial unique index. |
| `extracted_at` | `timestamptz` | |
| `fs_path` | `text` | Location in the blob tree. |
| `tsv` | `tsvector` | Generated from `content_text`. |

Indexes: `article_content_current_idx`, a unique index on `(article_id) WHERE
is_current`; `article_content_tsv_idx`, a GIN index on `tsv`;
`article_content_version_idx` on `(extractor_version) WHERE is_current AND NOT
immutable`, which is what keeps `tome reextract` from scanning
the whole archive.

#### Search language

The full-text index is fixed to the `english` configuration:

```sql
ALTER TABLE article_content ADD COLUMN tsv tsvector
  GENERATED ALWAYS AS (to_tsvector('english', content_text)) STORED;
```

`articles.language` is captured but unused by search. A per-row configuration
cannot be used in a generated column: `to_tsvector(regconfig, text)` is
immutable, but the `text::regconfig` cast needed to read a per-row config
column is only *stable*, so PostgreSQL rejects the expression.

The migration path, when the archive actually contains non-English content:
drop the generated column, keep a plain `tsv tsvector` maintained by the
application on write, and reindex.

### `feed_items`

A reference from one feed to one article.

| Column | Type | Notes |
|---|---|---|
| `id` | `bigserial` | Primary key. |
| `feed_id` | `bigint` | References `feeds`, cascading. |
| `article_id` | `bigint` | References `articles`. |
| `guid` | `text` | The feed's identifier, falling back to the canonical URL when absent. Unique per feed. |
| `feed_title`, `feed_summary` | `text` | As the feed gave them, kept separate from the extracted article's own title. |
| `feed_content` | `text` | The feed's own body (`content:encoded`), used by the extraction ladder's last rung. Distinct from the summary: this is sometimes the whole article, and is the only copy when a site goes down between publication and the next poll. |
| `seen_at` | `timestamptz` | |

### `article_state`

Per-user read state, separate from the article so that a story arriving through
three feeds has one state rather than three.

| Column | Type | Notes |
|---|---|---|
| `user_id`, `article_id` | `bigint` | Composite primary key. |
| `read`, `starred` | `boolean` | |
| `kept` | `boolean` | Keep this article's body and images permanently. Exempts it from [retention](retention.md), independently of starring — starring is a reaction, keeping is an instruction to the retention policy. |
| `saved_at` | `timestamptz` | Non-null marks a manual save, which is one of the per-reference provenance signals. Starring sets it too, which is what keeps a starred article reachable after the feed that introduced it is gone; unstarring leaves it alone. |
| `read_at` | `timestamptz` | The **first** time it was read, not the latest. Cleared when an article goes back to unread, so the column never claims a read that was undone. |

Rows are created on demand, not when an article is ingested. Polling writes none;
unread state is the absence of a row.

This table is also the second half of what makes an article visible to a reader: a
row here keeps an article reachable even after the feed that introduced it is
deleted. See [Scoping and access control](../explanation/scoping-and-access-control.md).

### `assets` and `article_assets`

Content-addressed images, shared globally.

| Column | Type | Notes |
|---|---|---|
| `sha256` | `text` | Primary key. Of the **original** bytes, before resizing or transcoding, so deduplication survives an encoder change. |
| `media_type` | `text` | Of the stored file: usually `image/avif`. |
| `byte_size` | `bigint` | Of the stored file, not the source. |
| `width`, `height` | `int` | After downscaling. |
| `fs_path` | `text` | Store-relative, `assets/sha256/a1/b2/…`. |
| `source_url` | `text` | Where it came from. Also used to avoid re-downloading an image already fetched for another article. |
| `fetched_at` | `timestamptz` | |

`article_assets` is the many-to-many join, and is how one image used by ten
articles is stored once and referenced ten times. See [Storage
layout](storage-layout.md#the-assets-tree).

### `domain_rules`

Per-domain extraction overrides.

| Column | Type | Notes |
|---|---|---|
| `domain` | `text` | Lowercased on write. A rule applies to subdomains unless a more specific rule exists. Unique per owner — `(domain, COALESCE(user_id, 0))`. |
| `user_id` | `bigint` | The reader whose rule this is, or `NULL` for the household's. Cascades on delete. |
| `content_selector` | `text` | CSS selector for the article body. Overrides the heuristics and the ratio check. |
| `strip_selectors` | `text[]` | Removed before extraction. |
| `requires_js` | `boolean` | Needs a headless render. **Household only.** |
| `user_agent` | `text` | Reserved; not yet read. **Household only.** |
| `rate_limit_rps` | `numeric` | Per-host request rate, loaded into the HTTP client at worker startup. **Household only.** |
| `notes` | `text` | Why the rule exists. |

**The two halves of a rule have different owners**, and a check constraint holds the
line: a row with a `user_id` may set the content and strip selectors only.

- **Extraction** — `content_selector`, `strip_selectors` — decides how a stored page
  becomes a body, and a body is per-reader. Two readers may hold different
  selectors for one host and each get their own extraction.
- **Fetching** — `requires_js`, `user_agent`, `rate_limit_rps` — decides how the page
  is *retrieved*, and it is retrieved once. A reader cannot ask for a browser render
  of a page already fetched without one, because there is nothing per-reader for the
  setting to act on.

A reader's rule wins over the household's for that reader, **even when the
household's names a more specific domain**: specificity orders a reader's own rules
against each other, not against somebody else's. Deleting your rule is how you go
back to what everybody else gets.

The household's rules are managed with
[`tome domain-rule`](cli.md#tome-domain-rule).

### `tags`, `article_tags`, `highlights`

User-scoped annotation.

| Table | Columns | Notes |
|---|---|---|
| `tags` | `id`, `user_id`, `name` | Unique per `(user_id, lower(name))` in effect: names are compared case-insensitively on write, so "Rust" and "rust" are one tag and the first spelling wins. |
| `article_tags` | `article_id`, `tag_id` | The join. It has no `user_id` of its own, so the scope has to be enforced from both sides — the article must be visible *and* the tag must be the reader's. |
| `highlights` | `id`, `article_id`, `user_id`, `quote`, `note`, `created_at` | A passage and an optional note. |

**Both are readable and neither is yet writable.** The web interface lists tags on
the Feeds page and serves `/tags/{id}` as a stream, but nothing in the application
creates a tag or attaches one — there is no control for it. `highlights` is not
read or written anywhere at all.

They are in the schema early because retrofitting a `user_id` onto an annotation
table means a migration over data that already exists, and because the shape is
what fixes the scoping rule while it is still cheap to get right.

Do not confuse tags with **categories**. A category is the folder an OPML import
put a feed in, stored as `feeds.category` and not a table of its own — a category
exists exactly as long as some feed claims one. Tags are per-article and per-reader;
categories are per-feed and come from somebody else's reader.

### `import_records`

Idempotency bookkeeping for imports.

| Column | Type | Notes |
|---|---|---|
| `user_id`, `source_name`, `source_id` | | Composite primary key. |
| `article_id` | `bigint` | |
| `imported_at` | `timestamptz` | |

`user_id` is part of the key because source-system ids are unique only within
one person's instance. Two users importing their own Wallabag libraries would
otherwise collide.

## River's tables

The job queue owns its own schema, migrated separately by `tome migrate`.
Those tables are River's to change on its release schedule and are not part of
this data model. `dbtest` does not truncate them.

## Entity relationships

```
users ──┬──< feeds ──< feed_items >── articles ──< article_content
        │                               │  │
        ├──< article_state >────────────┘  ├──< article_assets >── assets
        ├──< tags ──< article_tags >───────┤
        ├──< highlights >──────────────────┤
        └──< import_records >──────────────┘

domain_rules (global, unattached)
```

Reading it: a user owns feeds; feeds carry items; items reference articles;
articles are shared, and everything user-specific about one hangs off the user
side of that boundary.

## See also

- [Scoping and access control](../explanation/scoping-and-access-control.md)
  — which tables are per-user, which are shared, and why
