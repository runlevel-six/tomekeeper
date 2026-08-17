# Storage layout

The archive lives under `TOME_BLOB_ROOT`. This describes what is there and how
it is addressed.

The layout is designed to be navigated by a person with a file manager, ten
years from now, with this service gone. That is not a nice-to-have: it is
principle 2.4, and it is why the directories carry readable names rather than
only hashes.

## The tree

```
<TOME_BLOB_ROOT>/
  articles/
    2026/
      08/
        the-slow-decay-of-link-rot-a1b2c3d4/
          index.html      # standalone page, opens in a browser from disk
          meta.json       # the article record (see export-format.md)
          raw.html.gz     # the original fetched page, gzipped
  assets/
    sha256/
      a1/
        b2/
          a1b2c3d4….avif  # one image, content-addressed
```

## Article directories

`articles/<year>/<month>/<slug>-<hash>/`

**The date is when the article entered the archive, not when it was
published.** Publication dates are frequently missing and occasionally absurd,
and a tree keyed on them scatters a decade-old article saved today into a
directory nobody thinks to look in. First-seen makes the tree append-only, so
"what did I archive last August" is answerable by listing a directory.

**The slug** comes from the title, lowercased, with runs of non-alphanumeric
characters collapsed to a single hyphen and the whole truncated to 60
characters on a word boundary. Letters outside ASCII are kept — an archive of a
Greek or Japanese site should have readable directory names too. An article
with no title falls back to the last path segment of its URL.

**The hash** is the first 8 hex characters of the SHA-256 of the article's
canonical URL. It disambiguates two articles with the same title in the same
month, and it is stable: titles get edited, canonical URLs do not.

### Files

| File | Contents |
|---|---|
| `index.html` | A complete, standalone page. Inline CSS, no scripts, no external references of any kind. Images are referenced by relative path into the assets tree. |
| `meta.json` | The article record in the interchange format, including every asset with its source URL. See [Export format](export-format.md). |
| `raw.html.gz` | The original page exactly as fetched, gzipped. Absent for articles that were imported rather than fetched, or whose fetch failed. |

`index.html` and `meta.json` are **derived**: they are regenerated from the
database and the assets tree whenever an article is re-extracted or its images
are re-localized. Editing them by hand is not useful. `raw.html.gz` is not
derived — it is the only copy of what the origin actually served, and nothing
regenerates it.

## The assets tree

`assets/sha256/<first 2>/<next 2>/<full hash><extension>`

Images are **content-addressed by the SHA-256 of the original bytes**, before
any resizing or transcoding. Addressing the output instead would mean that
changing the encoder, the quality setting, or the target dimension turned every
image in the archive into a second copy of itself.

The two levels of prefix directory keep any single directory to a few thousand
entries. Some filesystems degrade badly with hundreds of thousands of files in
one place, and a file manager should be able to open a directory without
freezing.

An image used by ten articles is stored **once** and referenced ten times. This
is the single largest storage saving in the archive, because syndicated stories
repeat their illustrations constantly and images are most of what an archive
weighs.

## Relative paths

`index.html` at `articles/2026/08/slug-a1b2c3d4/` references an asset as:

```html
<img src="../../../../assets/sha256/a1/b2/a1b2c3….avif">
```

Four levels up, because the article directory is four levels deep. This is what
makes the page work when opened as a `file://` URL, where there is no root to
be relative to.

The copy of the body **in the database** uses a root-relative path instead
(`/assets/sha256/…`), because that is what a web server can route. The two
forms are the same references written for two different readers; the archive
writer converts one into the other.

A consequence worth knowing: **the archive can be moved or copied wholesale**
and every page still resolves. Nothing points at an absolute location.

## Images that were not localized

An image can fail to be localized, or be deliberately skipped. Either way it
keeps its original absolute URL in the page, and the article is marked
`assets_status = 'partial'` so the gap is visible rather than being discovered
years later as a broken image.

Deliberate skips, from §5.5:

| Rule | Why |
|---|---|
| Smaller than 10KB **and** under 100×100 | Tracking pixels, spacers, and icons. Both conditions must hold: a 4KB image that is 800px wide is a well-optimized diagram and is kept. |
| Data URIs | Already self-contained; the bytes are in the body. |
| SVG over 1MB | Usually a plotting tool that embedded its dataset. |
| Anything over 20MB | A malfunction, not a photograph. |
| Not decodable as an image | The server sent something else. |

## What images cost

Every image is downscaled so its long edge is at most **1600 pixels**, then
transcoded to **AVIF**, falling back to lossless **WebP**, falling back to the
original bytes.

The last fallback also applies when a transcode would be *larger* than the
source. The WebP encoder here is lossless-only, so a photograph re-encoded
losslessly is routinely bigger than the JPEG it came from — and this pipeline
exists to reduce storage, not to spend it.

### Measuring your archive

```sh
tome archive stats
```

```
articles              1,204
  fetched             1,180
  with a body         1,166
  partial images         31
body text            18.4 MB

images stored         2,140
image references      2,880
image bytes         214.7 MB
  deduplicated        740 references, about 74.2 MB not stored twice

per article         199.7 KB (body and images; excludes raw pages on disk)
per 1,000 articles  195.0 MB
```

Raw pages live on the filesystem rather than in the database, so size them
there:

```sh
du -sh "$TOME_BLOB_ROOT/articles"   # index.html, meta.json, raw.html.gz
du -sh "$TOME_BLOB_ROOT/assets"     # images
```

> **The numbers above are illustrative, not measured.** M3's acceptance
> criterion asks for storage across 1,000 real articles to be measured and
> recorded here, and that has to come from a real feed list — the mix of
> photo-heavy and text-only sources changes the answer by an order of
> magnitude. Run `tome archive stats` once your archive passes a thousand
> articles and replace this section with what it actually says.

## Backups

Two things to back up, with different characteristics:

| What | How | Notes |
|---|---|---|
| PostgreSQL | `pg_dump` | Small. It is an index over the files and can, in principle, be rebuilt from them. |
| `TOME_BLOB_ROOT` | File-level replication (restic, rsync, object storage) | Large, and almost entirely append-only — content-addressed assets are never rewritten, so incremental backups stay cheap. |

If you have to choose, back up the files. The database is a convenience; the
files are the archive.

## See also

- [Export format](export-format.md) — what `meta.json` contains
- [Why the filesystem is the archive](../explanation/why-the-filesystem-is-the-archive.md)
- [Configuration](configuration.md) — `TOME_BLOB_ROOT`
