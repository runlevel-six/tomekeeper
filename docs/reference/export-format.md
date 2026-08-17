# Export format

One format serves three purposes: it is what importers produce, what exports
emit, and what `meta.json` contains in every article's directory.

That symmetry is deliberate. Because export uses the same type
every importer consumes, export is exercised by every import test and cannot
quietly rot. For a project with one maintainer, a format that only breaks when
someone finally needs it is the format that loses the archive.

**Current version: 1.**

## The record

```json
{
  "schema_version": 1,
  "url": "https://journal.example.com/2026/08/link-rot",
  "title": "The Slow Decay of Link Rot",
  "author": "Dana Okonkwo",
  "site_name": "Example Journal",
  "language": "en",
  "published_at": "2026-08-01T09:30:00Z",
  "saved_at": "2026-08-17T12:00:00Z",
  "content_html": "<p>A link that worked in 2011…</p>",
  "raw_html_path": "raw.html.gz",
  "excerpt": "A link that worked in 2011 has roughly even odds…",
  "tags": ["archiving", "web"],
  "read": false,
  "starred": false,
  "archived": false,
  "extractor": "trafilatura",
  "extractor_version": "2",
  "word_count": 1204,
  "assets": [
    {
      "path": "../../../../assets/sha256/a1/b2/a1b2c3….avif",
      "source_url": "https://journal.example.com/images/decay-chart.png",
      "sha256": "a1b2c3…",
      "media_type": "image/avif",
      "byte_size": 41984,
      "width": 1600,
      "height": 900
    }
  ],
  "highlights": [
    { "quote": "Archiving is the only durable answer", "note": "the thesis" }
  ]
}
```

## Fields

| Field | Type | Notes |
|---|---|---|
| `schema_version` | int | Required. A reader must refuse a version it does not know. |
| `url` | string | Required. The article's URL. |
| `resolved_url` | string | Where a redirect led, when the source recorded one. |
| `source_id`, `source_name` | string | Identity in the system this came from, for idempotent re-import. Absent for articles this archive fetched itself. |
| `title`, `author`, `site_name`, `language` | string | Metadata, all optional. |
| `published_at`, `saved_at` | RFC 3339 | Optional. |
| `content_html` | string | The extracted body. Image references point at the paths in `assets`, relative to this file. |
| `raw_html_path` | string | Path to the original page, relative to this file. Not inlined — a decade of raw pages inside JSON would make every record unreadable in a text editor. |
| `excerpt` | string | Optional summary. |
| `tags` | string[] | |
| `read`, `starred`, `archived` | bool | Reader state. |
| `extractor`, `extractor_version` | string | What produced `content_html`, so an imported body can be told from an extracted one. |
| `immutable` | bool | The body must never be regenerated — typically an import that is the only surviving copy of a dead URL. |
| `word_count` | int | Derived, stored anyway: a reader browsing the files without this service running has no other way to get it. |
| `assets` | object[] | Localized images. |
| `highlights` | object[] | Marked passages. |

### Assets

| Field | Notes |
|---|---|
| `path` | Relative to the record's own directory, so a copied or moved archive still resolves. |
| `source_url` | Where it came from. Kept so a lost asset can be re-fetched, and so localization does not erase provenance. |
| `sha256` | Of the **original** bytes, before resizing or transcoding. |
| `media_type`, `byte_size`, `width`, `height` | Of the stored file. |

### Highlights

A highlight stores the **quoted text**, not a character range.

Ranges are brittle: they are offsets into one system's rendering of a body, and
they stop meaning anything the moment the body is re-extracted by a different
extractor. The quoted text survives that. Wallabag's annotations are converted
on import by matching the quote against the imported body, and ranges that do
not resolve are dropped rather than guessed at.

## Versioning

Bump `schema_version` for any change an older reader could misinterpret: a
removed field, a renamed field, or a changed meaning.

**Adding an optional field does not require a bump.** An older reader ignoring
a field it does not know about is the intended behavior, and bumping for
additions would make every archive look unreadable to every older build.

A reader that encounters a **newer** version than it knows must refuse the file
and say so, rather than reading the fields it recognizes. A newer version may
have changed what a familiar field means, and silently misreading an archive is
worse than not reading it.

### History

| Version | Change |
|---|---|
| 1 | Initial format. |

Every future bump gets a row here saying what changed and how to read the older
version. An archive contains files written by every version this service has
ever run.

## Stability

This format was designed with the archive writer rather than with the importers
that will consume it, because `meta.json` cannot exist without it. The importers
**consume** this format; they do not invent one. Any change they want is a
versioned format change with a row in the
table above.

## See also

- [Storage layout](storage-layout.md) — where these files live
- [Why the filesystem is the archive](../explanation/why-the-filesystem-is-the-archive.md)
