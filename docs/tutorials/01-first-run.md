# Tutorial 1: Your first run

In this tutorial you will start Tomekeeper on your own machine, subscribe it to
some feeds, and watch it collect articles.

It takes about 20 minutes. You will need Docker and Go 1.26 or later.

> **What you will have at the end:** a running service that polls your feeds,
> fetches each linked page, keeps the original, and extracts the readable
> article from it. There is no web interface yet — that is M4 — so the last step
> reads the archive with SQL.

## Step 1: Start PostgreSQL

Tomekeeper keeps everything in one database. Start one:

```sh
docker run -d --name tome-db \
  -e POSTGRES_USER=tome \
  -e POSTGRES_PASSWORD=tome \
  -e POSTGRES_DB=tome \
  -p 5432:5432 \
  postgres:16-alpine
```

Check that it is ready:

```sh
docker exec tome-db pg_isready -U tome
```

You should see `accepting connections`.

## Step 2: Build the binary

```sh
git clone https://github.com/runlevel-six/tomekeeper.git
cd tomekeeper
task build
```

You now have `./bin/tome`. Confirm it:

```sh
./bin/tome version
```

## Step 3: Configure it

Every setting is an environment variable. Set the five that matter here:

```sh
export TOME_DATABASE_URL='postgres://tome:tome@localhost:5432/tome?sslmode=disable'
export TOME_LOG_FORMAT='text'
export TOME_LOG_LEVEL='info'
export TOME_CONTACT_URL='https://example.com/about'
export TOME_BLOB_ROOT="$PWD/archive"
```

`TOME_BLOB_ROOT` is where fetched pages are stored. It must be an absolute path,
which is why this uses `$PWD` rather than a relative one — the archive should
not move because you started the service from a different directory.

`TOME_LOG_FORMAT=text` makes the output readable in a terminal; the default is
JSON, which is what you want in production.

`TOME_CONTACT_URL` goes into the `User-Agent` sent to every site you poll, so
that an operator who wants you to stop can find out who to ask. Put a real page
of your own there when you run this for more than a tutorial.

## Step 4: Create the schema

```sh
./bin/tome migrate
```

```
schema up to date; user "tome" is id 1
```

That created every table and the single user. Run it again — it prints the same
thing and changes nothing. Migrations are safe to repeat, which is why they run
on every deployment.

## Step 5: Subscribe to some feeds

Create a file called `my-feeds.opml`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head><title>Subscriptions</title></head>
  <body>
    <outline text="Go">
      <outline type="rss" text="The Go Blog"
               xmlUrl="https://go.dev/blog/feed.atom"
               htmlUrl="https://go.dev/blog/"/>
    </outline>
  </body>
</opml>
```

Look at what would be imported before importing it:

```sh
./bin/tome import-opml --dry-run my-feeds.opml
```

```
my-feeds.opml: 1 subscriptions (dry run, nothing written)

CATEGORY  TITLE        FEED URL
Go        The Go Blog  https://go.dev/blog/feed.atom
```

Nothing has been written yet. Now do it for real:

```sh
./bin/tome import-opml my-feeds.opml
```

```
my-feeds.opml: 1 added, 0 already subscribed
```

If you already use another feed reader, export its subscriptions as OPML and
import that file instead. A few hundred feeds import in a second or two.

## Step 6: Start the worker

```sh
./bin/tome worker
```

The worker checks for due feeds every minute, and once immediately at startup.
Within a few seconds you should see something like:

```
level=INFO msg="worker started"
level=INFO msg="polled feed" feed_id=1 items=10 new_items=10 new_articles=10 next_interval=30m0s
level=INFO msg="fetched article" article_id=1 bytes=48210 stored_bytes=9433 path=articles/2026/08/…
level=INFO msg="extracted article" article_id=1 extractor=trafilatura words=1204 characters=7331
```

Three things are happening. The feed is polled; each new article's page is
fetched and the original stored, compressed, on disk; and the readable body is
extracted from that stored copy.

Note `extractor=trafilatura`. That is the first rung of the extraction ladder
that produced an acceptable result — if a site needs a hand-written rule, this
is where you will see `readability` or a very short body instead.

Leave it running and watch. Poll it a second time — wait a minute — and you
will see nothing new, because the feed has not changed. That is the conditional
request working: the server answered "not modified" and sent no content at all.

Stop it with `Ctrl-C` when you have seen enough. It finishes what it is doing
before exiting.

## Step 7: See what you collected

In another terminal:

```sh
docker exec -it tome-db psql -U tome -d tome
```

```sql
SELECT title, url_canonical, published_at
FROM articles
ORDER BY published_at DESC NULLS LAST
LIMIT 10;
```

Every article the feed listed is there, with its canonical URL — tracking
parameters stripped, so the same story arriving through a second feed later
will match this row instead of creating another.

Check the feed's polling state too:

```sql
SELECT title, poll_interval, next_poll_at, consecutive_failures, etag IS NOT NULL AS has_etag
FROM feeds;
```

`has_etag` being true is why the second poll transferred nothing. `poll_interval`
will have adjusted itself based on what the poll found.

Now the part M2 added — the article text itself:

```sql
SELECT a.title, c.extractor_name, c.word_count, left(c.content_text, 300)
FROM articles a
JOIN article_content c ON c.article_id = a.id AND c.is_current
ORDER BY c.extracted_at DESC
LIMIT 3;
```

And where the originals went:

```sql
SELECT fetch_status, count(*) FROM articles GROUP BY fetch_status;
```

```sh
find archive/articles -name 'raw.html.gz' | head -3
zcat "$(find archive/articles -name 'raw.html.gz' | head -1)" | head -20
```

Those files are the point. The extracted body in the database is a *view* over
them, so a better extractor later can be applied to everything already
collected — see [Reprocess the
archive](../how-to/reprocess-the-archive.md).

Anything at `fetch_status = 'failed'` or `'skipped'` is worth a look:

```sql
SELECT url_canonical, fetch_status, fetch_error
FROM articles WHERE fetch_status IN ('failed', 'skipped');
```

`skipped` means the site's `robots.txt` said no. `failed` usually means a
paywall or a JavaScript-rendered page — the cases [a domain
rule](../how-to/add-a-domain-rule.md) exists for.

## Step 8: Run the server

In another terminal, with the same environment variables:

```sh
./bin/tome serve
```

```sh
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz
```

```json
{"status":"ok"}
{"status":"ready","checks":{"database":"ok"}}
```

There is no web interface yet — that is M4. What is running is the health
surface an orchestrator needs, and it is honest: stop the database and `/readyz`
starts reporting `503` while `/healthz` keeps returning `200`, because the
process is fine and only its dependency is not.

## Clean up

```sh
docker rm -f tome-db
rm -rf archive
```

That deletes the database and the stored pages.

## What next

- [How to troubleshoot a failing feed](../how-to/troubleshoot-a-failing-feed.md)
  — when a subscription stops producing articles
- [How to add a domain rule](../how-to/add-a-domain-rule.md) — when a site
  extracts badly
- [Extraction and versioning](../explanation/extraction-and-versioning.md) — why
  the original pages are kept
- [Why articles are the root entity](../explanation/why-articles-are-the-root-entity.md)
  — what those canonical URLs are actually for
- [Configuration](../reference/configuration.md) — every setting, including the
  polling intervals you just watched adapt
