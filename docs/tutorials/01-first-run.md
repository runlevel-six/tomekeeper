# Tutorial 1: Your first run

In this tutorial you will start Tomekeeper on your own machine, subscribe it to
some feeds, and watch it collect articles.

It takes about 20 minutes. You will need Docker and Go 1.26 or later.

> **What you will have at the end:** a running service that polls your feeds,
> fetches each linked page, extracts the readable article, downloads its images,
> and writes the result as a page you can open in a browser with everything
> switched off — plus a web interface to read it in, with search across the whole
> archive.
>
> You will read your first article twice: once as a file with the service stopped,
> which is the point of the whole design, and once in the reader.

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

The command line is used here because you are already in a terminal and have not
started the server yet. Once it is running, **Feeds → Import subscriptions** in
the web interface does the same thing with the same code.

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
level=INFO msg="localized assets" article_id=1 found=3 localized=3 failed=0 status=ok
```

Four things are happening. The feed is polled; each new article's page is
fetched and the original stored, compressed, on disk; the readable body is
extracted from that stored copy; and the article's images are downloaded,
downscaled, and written into the archive.

Image encoding is the slow part — a few seconds per picture — so an
illustrated article takes a moment to finish. It runs in the background and
nothing waits on it.

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

Now the part that matters — the article text itself:

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

And what it all costs:

```sh
./bin/tome archive stats
```

## Step 8: Open an article with everything switched off

This is the part that makes it an archive rather than a cache.

Stop the worker if it is still running, and stop the database too:

```sh
docker stop tome-db
```

Now find an archived article and open it:

```sh
find archive/articles -name index.html | head -5
xdg-open "$(find archive/articles -name index.html | head -1)"   # or: open, on macOS
```

The article renders, with its images, with no service running and no database
at all. Nothing is fetched from the network to display it — the page has inline
styles, no scripts, and its images resolve by relative path into
`archive/assets/`.

Look at what is beside it:

```sh
ls "$(dirname "$(find archive/articles -name index.html | head -1)")"
```

```
index.html   meta.json   raw.html.gz
```

`meta.json` is the article's record, readable in any text editor.
`raw.html.gz` is exactly what the server sent, kept so that a better extractor
later can be applied to articles already collected.

Start the database again before continuing:

```sh
docker start tome-db
```

## Step 9: Run the server

Give yourself a password first. `tome serve` never reads it — it checks against
the stored hash — so setting it belongs to `migrate`.

```sh
TOME_PASSWORD='choose something' ./bin/tome migrate
```

Then, in another terminal with the same environment variables:

```sh
./bin/tome serve
```

```sh
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz
```

```json
{"status":"ok"}
{"status":"ready","checks":{"database":"ok","schema":"ok"}}
```

That is the health surface an orchestrator needs, and it is honest: stop the
database and `/readyz` starts reporting `503` while `/healthz` keeps returning
`200`, because the process is fine and only its dependency is not.

## Step 10: read it in the browser

Open <http://localhost:8080> and sign in as `tome` with the password you just
set.

You should see the unread stream. Click an article to read it — the images are
served from your own archive, not from the original site. Try `j` and `k` to move
between entries, `o` to open one, and `s` to star it. Then, from inside an article,
`n` for the next one, `p` for the one before, and `u` to go back to the list you
came from. Search for a word you remember from one of the articles.

If a first import left you with hundreds of unread articles you have no intention
of reading, **Mark _n_ as read** at the top right of any list clears that list —
that list only, and it asks first, with the count. From a category page it marks the
category; from **Unread** it marks everything unread.

The category links above the list narrow it to one folder. On **Unread** that gives
you what is new in the comics and nothing else — including its own **Mark as read**,
so a folder you have fallen behind on can be cleared without touching the rest. On
**Everything** the same links take you to that folder's whole archive.

Three pages worth visiting once, because they are what makes the rest
maintainable:

- **Categories** groups your subscriptions by the folders your OPML export used,
  so you can read only the comics or only the tech feeds. If your export had no
  folders, everything lands in one bucket.
- **Feeds** shows every subscription with the last error it gave. A feed is never
  dropped silently. It also has **Check all feeds now**, which brings every feed's
  next poll forward — useful in a tutorial, where you do not want to wait fifteen
  minutes to see the adaptive interval do something.

  It is also where you add a feed by hand, in the form above the list. Try it with a
  site you read: paste the *site's* address rather than hunting for its feed URL,
  press **Test**, and the page reports the feed it found — following the site's own
  feed link if what you gave it was a web page. File it in a category and press
  **Add feed**.

  **Edit** on a row opens that subscription in the same form, which is how you
  correct an address that has moved, re-file a feed, or stop polling one without
  losing what it has already archived. Once the list is longer than a screen, the
  column headings sort it and the filter above it narrows it — including down to
  just the feeds that are failing.
- **Attention** lists anything that did not come through cleanly — usually a site
  that needs a [domain rule](../how-to/add-a-domain-rule.md), sometimes a page no
  extractor will ever read. Nothing there is lost; the stored page can be
  re-extracted once a rule exists.

### Save something nothing subscribed to

Go to **Saved** and paste a link to any article — something a colleague sent you,
not something a feed brought. The page is queued, and the worker fetches, extracts
and archives it exactly as it does a feed item: same pipeline, same
standalone-`index.html` on disk. This is the half of the tool that replaces a
read-later app, and it is why the article rather than the feed item is the root
entity — a saved page needs no feed to hang off.

### Make it yours

**Settings** has six palettes plus the neutral one it starts with. They are not decoration: this is a page you look
at for hours, and each palette is a field color, a metallic, and a parchment taken
from the archive seal's own design. Each can follow the system light/dark
preference or be pinned to one. The choice is stored against your user and rendered
into the page server-side, so there is no flash of the wrong colors on load.

The same page is where retention would be explained if you turned it on. You have
not: it is off by default, and until you set `TOME_RETAIN_AFTER_READ` nothing in
this archive is ever released.

### Try it as an installed app

Worth doing once, because the interface changes shape. Install it from your
browser's menu — in Chrome, the icon in the address bar; on iOS, *Add to Home
Screen*. Opened from the home screen there is no address bar, no back button and no
reload, so the page draws its own: a tab bar along the bottom, a reload control in
the header, and a way back to the list from every article. The unread count leads
the window title, and on platforms that support it rides the app icon as a badge.

If signing in fails, the page says whether a password has been set at all. Every
other rejection reads the same on purpose, and the reason is in the server's log.

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
- [Why the filesystem is the archive](../explanation/why-the-filesystem-is-the-archive.md)
  — what you just did in step 8, and why it is the point
- [Extraction and versioning](../explanation/extraction-and-versioning.md) — why
  the original pages are kept
- [Why articles are the root entity](../explanation/why-articles-are-the-root-entity.md)
  — what those canonical URLs are actually for
- [Configuration](../reference/configuration.md) — every setting, including the
  polling intervals you just watched adapt
