# Import from Wallabag

You have a Wallabag library — a read-later archive of pages you saved by hand —
and you want it in Tomekeeper alongside what your feeds bring.

This takes two commands and then some patience while the images arrive.

## Before you start

- Tomekeeper set up and migrated, with a user (`tome migrate` has run).
- The worker running, or ready to run. The import itself fetches nothing; the
  worker is what collects the images and the pages Wallabag never got.

## 1. Export from Wallabag

In Wallabag: **Settings → Export → JSON**. Save the file somewhere the `tome`
binary can read.

Take the JSON export rather than reading Wallabag's database directly. It is a
stable artifact you can re-run, inspect in an editor, and keep — and Wallabag's
schema has drifted across releases in ways an export does not.

## 2. Look before you leap

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
```

Nothing is written. Read the two lines that surprise people:

**"without a body"** is not a count of things that went wrong here. When
Wallabag's own fetch failed, it stored a sentence of its own prose in the content
field — *"wallabag can't retrieve contents for this article"* — and those records
have no article in them. The import recognizes that message, refuses to store it
as a body, and queues the page for Tomekeeper to fetch instead. So this number is
the number of pages your library was quietly missing, and Tomekeeper is about to
try to get them.

**"images to fetch"** is work that happens *after* the import, in the worker, at
its own pace. Wallabag only downloads images when `download_images` is enabled;
with it off, every picture in your library is still a reference to the site it came
from. Those references are stored and the worker localizes them one by one.

If the report mentions images held **inside the wallabag installation**, that
instance did have image downloading on. Those files are on that machine and
Tomekeeper cannot reach them — the pictures for those articles come from the
original sites or not at all.

## 3. Import

```console
$ tome import wallabag.json
...
imported 385 articles: 341 bodies stored, 44 queued for fetching
```

The report prints again first — the command always reads the file twice, so a
truncated export fails before anything is written — and then the import runs.

Your library is now under **Saved**, dated when *Wallabag* saved each page rather
than today, so the order you built up over years is still the order you see.

## 4. Wait for the images

Nothing else is needed, but nothing is finished either. Watch it arrive:

```console
$ tome archive stats
```

Until the worker reaches an article, it shows its text with the images missing —
they are not loaded from the original site, ever, so a picture that is not archived
yet is a gap rather than a slow load. On a library with a couple of thousand images
this takes hours, and it is deliberately not faster: see
[politeness and rate limiting](../explanation/politeness-and-rate-limiting.md).

Articles whose page could not be fetched appear under **Attention**. For an
old library expect a real number of them — a decade-old save is a decade of
opportunity for a URL to stop working. Nothing is lost that Wallabag had: an
article Wallabag *did* hold keeps that body regardless of whether the fetch works
now.

## Running it again

Safe, and the way to pick up saves you made after the export:

```console
$ tome import wallabag.json
imported 0 articles: 0 bodies stored, 0 queued for fetching
385 records were already imported and were left alone.
```

Re-importing does not duplicate anything, and does not undo anything you have done
here. Tags you added stay, highlights are not doubled, and an article you have read
in Tomekeeper stays read even though Wallabag still has it as unread.

That last point is worth knowing in the other direction too: the import can *add*
read and starred state but never remove it. If you have been reading in both places,
the union is what you get.

## What if it goes wrong halfway

Run it again. Every write is idempotent and the bookkeeping record for each article
is written last, so an import interrupted by a lost database connection or a
`Ctrl-C` leaves finished articles finished and unfinished ones untouched. A second
run completes the job.

If a single record is unreadable, the import says so with its position in the file
and carries on with the rest:

```
  ! record 4102: record 4102 of wallabag.json: reading tags: ...
```

If the *file* is unreadable — truncated, or corrupt in a way that loses the
parser's place — the import stops and writes nothing at all, because everything
past that point is unknown rather than merely broken.

## Where it all went

| What | Where |
|---|---|
| The articles | **Saved**, and in **Unread** until you read them |
| The bodies Wallabag had | Stored immutably; `tome reextract` will never overwrite them |
| Pages Wallabag was missing | Queued for fetching; then in the archive like anything else |
| Tags | Tags, alongside any you add here |
| Annotations | Highlights, matched to the body by their quoted text |
| Failures | **Attention** |

For what an import does to the data model — and why imported bodies are immutable
— see [the CLI reference](../reference/cli.md#tome-import) and
[the data model](../reference/data-model.md).
