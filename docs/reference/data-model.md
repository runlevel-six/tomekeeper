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

The single v1 user, created by `tome migrate` from `TOME_USERNAME`. Multi-user
is M9; the schema is user-scoped from the start regardless.

| Column | Type | Notes |
|---|---|---|
| `id` | `bigserial` | Primary key. The seed user is always `1`. |
| `username` | `text` | Unique. |
| `password_hash` | `text` | Empty until authentication lands (M4). |
| `api_key` | `text` | Unique, nullable. MD5 of `username:password` for the Fever API (M5). It cannot be derived from `password_hash`, so it is written whenever the password is set. |
| `created_at` | `timestamptz` | |

### `feeds`

A user's subscriptions, and everything the poller needs to know about each.

| Column | Type | Notes |
|---|---|---|
| `id` | `bigserial` | Primary key. |
| `user_id` | `bigint` | References `users`. Cascades on delete. |
| `feed_url` | `text` | The URL polled. Unique per user. Stored exactly as imported — **not** canonicalized, because a query parameter on a feed endpoint may select which feed is served. |
| `site_url` | `text` | The human-readable site. Used as the base for resolving relative entry links. |
| `title` | `text` | Never empty; falls back to the feed URL. |
| `category` | `text` | From the OPML folder. Nested folders are joined with `/`. |
| `etag`, `last_modified` | `text` | Conditional-GET validators from the last successful poll. |
| `poll_interval` | `interval` | Current adaptive interval. |
| `next_poll_at` | `timestamptz` | When this feed becomes due. Defaults to `now()`, so a new subscription is polled promptly. |
| `last_polled_at`, `last_success_at` | `timestamptz` | A feed with a recent poll but a stale success is failing. |
| `consecutive_failures` | `int` | Reset to 0 by any success, including a 304. |
| `last_error` | `text` | Preserved after disabling, so the failure can be diagnosed. |
| `disabled` | `boolean` | Set automatically at `TOME_FEED_FAILURE_THRESHOLD`. |

Indexes: `UNIQUE (user_id, feed_url)`; `feeds_due_idx` on `(next_poll_at) WHERE
NOT disabled`, which is the scheduler's hot path.

### `articles`

The root entity. A feed item, a manual save, and an imported entry are all
*references to* an article.

| Column | Type | Notes |
|---|---|---|
| `id` | `bigserial` | Primary key. |
| `url_canonical` | `text` | Unique. Produced by `internal/urlcanon`. This constraint is what makes deduplication work. |
| `url_original` | `text` | As first seen, tracking parameters intact. |
| `title`, `author`, `site_name`, `language` | `text` | Nullable. Filled from whichever reference first supplied each; existing values are never overwritten. |
| `published_at` | `timestamptz` | |
| `first_seen_at` | `timestamptz` | |
| `raw_blob_sha` | `text` | SHA-256 of the original fetched bytes. Null for imports with no original. |
| `raw_blob_path` | `text` | Where the gzipped page is in the blob store. Stored rather than derived: a title can change on a later poll, and a derived path would then point at nothing. |
| `raw_fetched_at` | `timestamptz` | |
| `fetch_status` | `text` | `pending`, `ok`, `failed`, `skipped`. Constrained by `CHECK`. M1 leaves everything `pending`. |
| `fetch_error` | `text` | |
| `assets_status` | `text` | `pending`, `ok`, `partial`, `none`. Constrained by `CHECK`. `partial` means at least one image could not be localized (M3). `pending` is strictly transient: an article whose pipeline ended without a body is set to `none` when the failure is recorded, because the asset scheduler joins the current content row and could never reach it otherwise. |

**There is deliberately no `origin` or `immutable` column here.** This row is
shared by every user, so "how did this arrive" has no single answer.
Per-reference provenance lives in `feed_items`, `import_records`, and
`article_state.saved_at`; per-body provenance lives in
`article_content.content_origin`.

Additional indexes from M2: `articles_awaiting_extraction_idx` on
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
immutable`, which is what keeps `tome reextract --since-version` from scanning
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
| `saved_at` | `timestamptz` | Non-null marks a manual save, which is one of the per-reference provenance signals. |
| `read_at` | `timestamptz` | |

Rows are created on demand, not when an article is ingested. M1 writes none;
unread state is the absence of a row.

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
| `domain` | `text` | Unique. Lowercased on write. A rule applies to subdomains unless a more specific rule exists. |
| `content_selector` | `text` | CSS selector for the article body. Overrides the heuristics and the ratio check. |
| `strip_selectors` | `text[]` | Removed before extraction. |
| `requires_js` | `boolean` | Needs a headless render. No effect until M8. |
| `user_agent` | `text` | Reserved; not yet read. |
| `rate_limit_rps` | `numeric` | Per-host request rate, loaded into the HTTP client at worker startup. |
| `notes` | `text` | Why the rule exists. |

Global and admin-only: how to extract a site's articles is a technical fact
about that site, identical for every reader. Managed with
[`tome domain-rule`](cli.md#tome-domain-rule).

### `tags`, `article_tags`, `highlights`

User-scoped annotation. `tags` is unique per `(user_id, name)`; `article_tags`
joins articles to tags, so the user scope travels via the tag. Unused until M4.

### `import_records`

Idempotency bookkeeping for imports (M6).

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
