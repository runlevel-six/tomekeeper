# Tutorial 2: Bring your reading with you

In this tutorial you will move an existing reading setup into Tomekeeper: the
subscriptions from a feed reader, and the saved articles from a read-later app.

It takes about 30 minutes, most of which is waiting while the worker fetches things.
You will need [Tutorial 1](01-first-run.md) finished — a running service with a
database, a user, and the worker available.

> **What you will have at the end:** your own feeds and your own saved articles in
> one archive, with the reading state you already had, and a clear picture of what
> came across intact and what did not.
>
> You will also learn the two things that surprise everyone: how many of your saved
> articles your old reader never actually held, and why the pictures arrive hours
> after the words.

## Step 1: Export from the reader you have

Every feed reader exports OPML. It is usually under settings, and usually called
"subscriptions", "OPML", or "export".

| Reader | Where |
|---|---|
| FreshRSS | Settings → Import/export → Export OPML |
| Miniflux | Settings → Feeds → Export |
| Feedly, Inoreader, NewsBlur | Organize/settings → OPML export |

Save it somewhere the `tome` binary can read. The file is small — a few hundred feeds
is under 100KB.

## Step 2: See what it holds before importing it

```sh
tome import-opml --dry-run subscriptions.opml
```

```
subscriptions.opml: 74 subscriptions (dry run, nothing written)

CATEGORY    TITLE                FEED URL
Technology  Example Engineering  https://engineering.example.com/feed.xml
Comics      A Daily Strip        https://strip.example.com/rss
...
```

Nothing has been written. This is a good moment to notice feeds you stopped caring
about years ago — but you do not have to prune them here, and there is an argument
for not bothering: a feed that has been dead for two years shows up in **Attention**
within a day of importing, with the reason, which is a better list than one you
assemble from memory.

The folders in your OPML become categories. Nested folders are joined with a slash,
so a feed in *News → Local* arrives as `News/Local`.

## Step 3: Import them

```sh
tome import-opml subscriptions.opml
```

```
subscriptions.opml: 74 added, 0 already subscribed
```

Re-running this is safe: subscriptions are keyed by URL, so a second run updates
titles and categories and creates nothing.

The web interface does the same thing under **Feeds → Import subscriptions**, sharing
this command's code. Use whichever is nearer.

## Step 4: Let the worker fill it in

```sh
tome worker
```

The first poll of a fresh subscription list is the busiest this archive ever gets.
Every feed is due at once, and a feed that has been running for years may list its
whole back catalog on the first fetch — so a "74 feed" import can produce well over
a thousand articles.

Leave it running. Watch the shape of it:

```
level=INFO msg="polled feed" feed_id=12 items=280 new_items=280 new_articles=280
level=INFO msg="fetched article" article_id=431 bytes=48210 stored_bytes=9433
level=INFO msg="extracted article" article_id=431 extractor=trafilatura words=1204
level=INFO msg="localized assets" article_id=431 found=3 localized=3 failed=0
```

**First-run volume overstates steady state, considerably.** Once the back catalogs
are in, a day's polling of the same list is dozens of articles rather than thousands.
Do not size anything on what you see today.

## Step 5: Bring your saved articles

If you use a read-later app, its library is the other half of your reading. Wallabag
exports JSON: **Settings → Export → JSON**.

Look before you commit:

```sh
tome import --dry-run wallabag.json
```

```
wallabag.json: 385 records from wallabag (dry run, nothing written)

  new               385
  already imported  0
  duplicate URLs    12   already in the archive; the import adds a reference, not a copy
  without a body    43   42 of these hold wallabag's own fetch-failure message; this archive will fetch them itself
  with images       201  2135 images to fetch and archive
  tags              0
  highlights        0
```

Three lines here are worth understanding, because they are the ones people
misread.

**"duplicate URLs"** are articles a feed already brought you. They are not skipped and
not duplicated: the import becomes another reference to the article that is already
there. One page, one stored copy, one set of images — which is why the archive counts
articles rather than saves.

**"without a body"** is not a count of things that just went wrong. When Wallabag's
own fetch failed, it stored a sentence of its own prose where the article should be.
Tomekeeper recognizes that message, refuses to store it as a body, and queues the page
to fetch for itself. So this number is how many pages your old reader was quietly
missing — and it is about to try to get them.

**"images to fetch"** is work that happens afterwards, in the worker. Wallabag only
downloads images when its instance was configured to; with that off, every picture in
your library is still a reference to the site it came from.

Then do it:

```sh
tome import wallabag.json
```

```
imported 385 articles: 341 bodies stored, 44 queued for fetching
```

Or upload the file at **Saved → Import a library**, with the same report and a
**Report only** checkbox for the dry run.

## Step 6: Wait, and watch it fill in

This is the part that takes real time, and it is worth watching once so that the
shape of it is familiar:

```sh
tome archive stats
```

Articles appear immediately with their text. Their images arrive over the following
hours, one polite request at a time — the archive never loads a picture from the
original site, so an article whose images are still queued shows correctly-sized
blank frames and a badge saying so. That is a gap being filled, not a bug.

## Step 7: Look at what did not come through

```
http://localhost:8080/attention
```

Expect a real number of entries after importing a long-lived library. A decade-old
save is a decade of opportunity for a URL to stop working, and the queue tells you
which kind of failure each one is:

- **failed** with an HTTP status — the page is gone, or refused us.
- **skipped** — the site's `robots.txt` asked not to be fetched. Nothing is wrong;
  the original link still works.
- **images incomplete** — the text is here, some pictures are not.

Nothing in this list is lost. An article Wallabag *did* hold keeps that body whether
or not the fetch works now.

If several entries share a site, that site probably needs an extraction rule — each
row links straight to the form, and [Add a domain rule](../how-to/add-a-domain-rule.md)
walks through writing one.

## Step 8: Read something you saved in 2019

Go to **Saved**. Your library is there, dated when *you* saved each page rather than
when you imported it, so the order you built up over years is the order you see.

Open something old. The images are archived locally; the original site may not even
exist. That is the whole point of the exercise.

## What you have now

| | |
|---|---|
| Your subscriptions | Polling on an adaptive schedule, with health visible on **Feeds** |
| Your saved articles | Under **Saved**, with their original dates |
| Everything both of them brought | Deduplicated to one article per page |
| Anything either was missing | Queued, fetched, and reported in **Attention** |

Importing either file again is safe. That is the intended way to pick up whatever you
saved after the export — run it again, and the report will be almost entirely
"already imported".

## Next

- [Export everything](../how-to/export-everything.md) — now that it is all in one
  place, take a copy of it.
- [Add a domain rule](../how-to/add-a-domain-rule.md) — for the sites in
  **Attention** that need a hand.
- [Import from Wallabag](../how-to/import-from-wallabag.md) — the reference version
  of step 5, with the details this tutorial skipped.
