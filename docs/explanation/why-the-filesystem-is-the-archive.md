# Why the filesystem is the archive

Most applications treat the database as the source of truth and the filesystem
as a place to put blobs that are inconvenient to store in it. Tomekeeper
inverts that. **The files are the archive. The database is an index over
them.**

That sentence is easy to write and has consequences that show up in almost
every design decision in the project.

## The failure being designed against

A feed reader that dies costs you nothing. You re-import your OPML into a new
one and lose a list of subscriptions you could have rebuilt from memory.

An archive that dies costs you ten years of reading. And archives do not
usually die dramatically — they die because the schema changed and the
migration was never written, or the binary stopped compiling against a
dependency that moved on, or the maintainer stopped, or the format was only
ever readable by one program and that program no longer runs anywhere.

So the question is not "how do I keep this working" but "what survives me not
keeping it working". The answer has to be a format that requires no software in
particular.

## What that means concretely

Every article is a directory:

```
articles/2026/08/the-slow-decay-of-link-rot-a1b2c3d4/
    index.html      # opens in any browser, from disk, offline
    meta.json       # the record, readable in a text editor
    raw.html.gz     # exactly what the server sent
```

`index.html` has **no external references at all**. Inline CSS, no scripts, no
webfonts, no stylesheet links. Images resolve by relative path into a shared
`assets/` tree. Double-click it in a file manager in 2036, with this service
long gone and Postgres long gone, and you get the article.

That property is enforced by a test that opens a generated page from a
temporary directory and checks every image against the filesystem. §8 of the
plan calls that test non-negotiable, and it is the one that would tell us the
archive had quietly become a cache.

## Why relative paths matter more than they look

An image is stored once and shared by every article that uses it, at
`assets/sha256/a1/b2/…`. The page four levels down references it as
`../../../../assets/sha256/a1/b2/…`.

The alternative — a root-relative `/assets/…` — is what the web UI uses, and it
is fine for a running server. It is useless to a browser reading
`file:///home/someone/archive/articles/2026/08/…/index.html`, where there is no
root.

The relative form also means **the archive can be copied or moved wholesale**.
Nothing in it points at an absolute location, so `rsync -a` to a new disk, a
new machine, or a USB stick handed to someone produces a working archive rather
than a working directory structure full of broken pages.

## The database earns its place differently

Postgres is not decoration. It holds read state, subscriptions, search, the job
queue — everything that makes this a usable reader rather than a pile of files.
Losing it would be genuinely painful.

But it is *recoverable*. `meta.json` beside every article carries the record in
full, so an index can in principle be rebuilt from the tree. The inverse is not
true: no amount of database would reconstruct a page whose bytes were never
kept.

This asymmetry is why the two are backed up differently, and why — if you have
to choose — you back up the files.

## The costs, honestly

**Duplication.** The body exists twice: as `content_html` in Postgres, for
search and the web UI, and inside `index.html` on disk. That is deliberate
redundancy in a system whose whole purpose is surviving partial loss, but it is
real duplication and it is worth naming.

**Regeneration.** `index.html` and `meta.json` are derived, so a re-extraction
has to rewrite them. That is a write per article per reprocess, which is why
re-extraction is a queued background job rather than something done inline.

**Filesystem limits.** Hundreds of thousands of small files is a shape some
filesystems handle badly. The two levels of hash prefix in `assets/` and the
year/month split in `articles/` exist for that, and they are also what keeps a
directory openable by a human without their file manager freezing.

**No object storage yet.** The `BlobStore` interface has an S3 implementation
in mind, and nothing in the code assumes a local disk beyond the filesystem
implementation. But an S3-backed archive is not one you can open in a file
manager, so it would be a different tradeoff — cheaper and less durable in the
way that matters here.

## What is deliberately not stored

The plan's non-goals are non-goals partly because of this principle. Full-page
visual snapshots, WARC captures, and PDF renderings are all formats that need
particular software to read. An HTML file in a folder needs a browser, and
browsers will read HTML for as long as there is a web.

## See also

- [Storage layout](../reference/storage-layout.md) — the tree, in detail
- [Export format](../reference/export-format.md) — what `meta.json` holds
- [Extraction and versioning](extraction-and-versioning.md) — why bodies are
  regenerable and raw pages are not
