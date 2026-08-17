-- Initial schema. See docs/reference/data-model.md.
--
-- Migrations are append-only: this file must never be edited once it has run
-- anywhere. Corrections are new migrations.

-- +goose Up

-- One user in v1, seeded by `tome migrate`. The schema is user-scoped from the
-- start because retrofitting access control is the expensive part of
-- multi-tenancy, not retrofitting columns.
CREATE TABLE users (
  id            bigserial PRIMARY KEY,
  username      text NOT NULL UNIQUE,
  -- Empty until authentication lands (M4). The Fever API key is MD5 of
  -- username:password and must be written whenever the password is set, since
  -- it cannot be derived from the hash afterwards.
  password_hash text NOT NULL DEFAULT '',
  api_key       text UNIQUE,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE feeds (
  id              bigserial PRIMARY KEY,
  user_id         bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  feed_url        text NOT NULL,
  site_url        text,
  title           text NOT NULL,
  category        text,
  -- conditional GET state
  etag            text,
  last_modified   text,
  -- polling state
  poll_interval   interval NOT NULL DEFAULT '1 hour',
  next_poll_at    timestamptz NOT NULL DEFAULT now(),
  last_polled_at  timestamptz,
  last_success_at timestamptz,
  consecutive_failures int NOT NULL DEFAULT 0,
  last_error      text,
  disabled        boolean NOT NULL DEFAULT false,
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, feed_url)
);

-- The scheduler's hot path: "which feeds are due right now".
CREATE INDEX feeds_due_idx ON feeds (next_poll_at) WHERE NOT disabled;

-- The root entity. A feed item, a manual save, and an imported entry are all
-- references TO an article; the article itself is shared by every user, which
-- is what makes deduplication of syndicated content free.
CREATE TABLE articles (
  id              bigserial PRIMARY KEY,
  url_canonical   text NOT NULL UNIQUE,
  url_original    text NOT NULL,
  title           text,
  author          text,
  site_name       text,
  language        text,
  published_at    timestamptz,
  first_seen_at   timestamptz NOT NULL DEFAULT now(),
  -- No origin/immutable columns: this row is shared, so "how did this arrive"
  -- has no single answer. Per-reference provenance lives in feed_items,
  -- import_records, and article_state.saved_at; per-body provenance lives in
  -- article_content.content_origin.
  raw_blob_sha    text,
  raw_fetched_at  timestamptz,
  fetch_status    text NOT NULL DEFAULT 'pending'
    CHECK (fetch_status IN ('pending', 'ok', 'failed', 'skipped')),
  fetch_error     text,
  assets_status   text NOT NULL DEFAULT 'pending'
    CHECK (assets_status IN ('pending', 'ok', 'partial', 'none'))
);

-- The fetch worker's hot path (M2).
CREATE INDEX articles_pending_fetch_idx ON articles (first_seen_at)
  WHERE fetch_status = 'pending';

-- Derived, versioned extraction. Extraction quality only improves, so a body
-- is a view over the raw fetch rather than the thing itself.
CREATE TABLE article_content (
  id                bigserial PRIMARY KEY,
  article_id        bigint NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
  extractor_name    text NOT NULL,
  extractor_version text NOT NULL,
  -- Provenance of this body, not of the article.
  content_origin    text NOT NULL,
  -- True for imported bodies, which may be the only surviving copy of a dead
  -- URL. The re-extract pass skips these and they are never overwritten.
  immutable         boolean NOT NULL DEFAULT false,
  content_html      text NOT NULL,
  content_text      text NOT NULL,
  word_count        int,
  is_current        boolean NOT NULL DEFAULT true,
  extracted_at      timestamptz NOT NULL DEFAULT now(),
  fs_path           text
);

CREATE UNIQUE INDEX article_content_current_idx
  ON article_content (article_id) WHERE is_current;

-- Full-text search. The 'english' configuration is fixed: a per-row config
-- cannot be used in a generated column, because the text::regconfig cast is
-- only stable, not immutable. See docs/reference/data-model.md.
ALTER TABLE article_content ADD COLUMN tsv tsvector
  GENERATED ALWAYS AS (to_tsvector('english', content_text)) STORED;
CREATE INDEX article_content_tsv_idx ON article_content USING GIN (tsv);

CREATE TABLE feed_items (
  id           bigserial PRIMARY KEY,
  feed_id      bigint NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
  article_id   bigint NOT NULL REFERENCES articles(id),
  guid         text NOT NULL,
  feed_title   text,
  feed_summary text,
  seen_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (feed_id, guid)
);

CREATE INDEX feed_items_article_idx ON feed_items (article_id);
CREATE INDEX feed_items_feed_seen_idx ON feed_items (feed_id, seen_at DESC);

-- Per-user read state, kept separate so an article arriving via three feeds
-- has one state rather than three.
CREATE TABLE article_state (
  user_id     bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  article_id  bigint NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
  read        boolean NOT NULL DEFAULT false,
  starred     boolean NOT NULL DEFAULT false,
  saved_at    timestamptz,
  read_at     timestamptz,
  PRIMARY KEY (user_id, article_id)
);

CREATE INDEX article_state_unread_idx ON article_state (user_id) WHERE NOT read;
CREATE INDEX article_state_starred_idx ON article_state (user_id) WHERE starred;

-- Content-addressed assets, shared globally like articles.
CREATE TABLE assets (
  sha256       text PRIMARY KEY,
  media_type   text NOT NULL,
  byte_size    bigint NOT NULL,
  width        int,
  height       int,
  fs_path      text NOT NULL,
  fetched_at   timestamptz NOT NULL DEFAULT now(),
  source_url   text
);

CREATE TABLE article_assets (
  article_id  bigint NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
  sha256      text NOT NULL REFERENCES assets(sha256),
  PRIMARY KEY (article_id, sha256)
);

-- Per-domain extraction overrides. Global and admin-only: extraction rules are
-- technical, not personal.
CREATE TABLE domain_rules (
  id               bigserial PRIMARY KEY,
  domain           text NOT NULL UNIQUE,
  content_selector text,
  strip_selectors  text[],
  requires_js      boolean NOT NULL DEFAULT false,
  user_agent       text,
  rate_limit_rps   numeric,
  notes            text
);

CREATE TABLE tags (
  id      bigserial PRIMARY KEY,
  user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name    text NOT NULL,
  UNIQUE (user_id, name)
);

CREATE TABLE article_tags (
  article_id bigint NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
  tag_id     bigint NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY (article_id, tag_id)
);

CREATE TABLE highlights (
  id          bigserial PRIMARY KEY,
  article_id  bigint NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
  user_id     bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  quote       text NOT NULL,
  note        text,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX highlights_article_idx ON highlights (user_id, article_id);

-- Import bookkeeping for idempotent re-import. User-scoped because source
-- system ids are unique only within one person's instance.
CREATE TABLE import_records (
  user_id     bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  source_name text NOT NULL,
  source_id   text NOT NULL,
  article_id  bigint NOT NULL REFERENCES articles(id),
  imported_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, source_name, source_id)
);

-- +goose Down
DROP TABLE IF EXISTS import_records;
DROP TABLE IF EXISTS highlights;
DROP TABLE IF EXISTS article_tags;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS domain_rules;
DROP TABLE IF EXISTS article_assets;
DROP TABLE IF EXISTS assets;
DROP TABLE IF EXISTS article_state;
DROP TABLE IF EXISTS feed_items;
DROP TABLE IF EXISTS article_content;
DROP TABLE IF EXISTS articles;
DROP TABLE IF EXISTS feeds;
DROP TABLE IF EXISTS users;
