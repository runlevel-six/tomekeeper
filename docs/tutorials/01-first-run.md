# Tutorial 1: Your first run

In this tutorial you will start Tomekeeper on your own machine, subscribe it to
some feeds, and watch it collect articles.

It takes about 20 minutes. You will need Docker and Go 1.26 or later.

> **What you will have at the end:** a running service that polls your feeds and
> records every article it finds. It does not yet fetch or display the article
> text — that arrives in the next milestone. What you can verify here is
> ingestion, which is the foundation everything else sits on.

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

Every setting is an environment variable. Set the four that matter here:

```sh
export TOME_DATABASE_URL='postgres://tome:tome@localhost:5432/tome?sslmode=disable'
export TOME_LOG_FORMAT='text'
export TOME_LOG_LEVEL='info'
export TOME_CONTACT_URL='https://example.com/about'
```

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
```

`new_articles=10` means ten articles are now in your archive.

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

One more thing worth noticing:

```sql
SELECT fetch_status, count(*) FROM articles GROUP BY fetch_status;
```

Everything is `pending`. Ingestion has done its job and handed off; fetching and
extracting the article text is the next milestone's work.

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
```

That deletes the database and everything in it.

## What next

- [How to troubleshoot a failing feed](../how-to/troubleshoot-a-failing-feed.md)
  — when a subscription stops producing articles
- [Why articles are the root entity](../explanation/why-articles-are-the-root-entity.md)
  — what those canonical URLs are actually for
- [Configuration](../reference/configuration.md) — every setting, including the
  polling intervals you just watched adapt
