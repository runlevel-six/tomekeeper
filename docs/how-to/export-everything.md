# Export everything

You want a copy of the archive that does not depend on this service, this database,
or this machine — to move it, to back it up, or to satisfy yourself that you can.

## The short version

```sh
tome export --out archive.json
cp -a "$TOME_BLOB_ROOT" ./archive-files
```

Two commands because the archive is two things: what the database knows, and the
files on disk. Either alone is incomplete, and the first command says so when the
export references any images.

## What is in the file

Every article you can see, with its metadata, your reading state, tags, highlights,
and the body currently shown. It is a JSON array, indented, meant to be opened in an
editor — see [Export format](../reference/export-format.md) for the record.

Images and stored original pages are **referenced by path**, not carried. A decade
of pictures base64'd into JSON is not a document anybody can open, and they are
already sitting in `TOME_BLOB_ROOT` as ordinary files.

## Restoring it

Into an empty database:

```sh
tome migrate
tome import archive.json
```

The import reads the file twice — once to report, once to write — so a truncated
copy fails before anything is written. Re-running is safe: the second run recognizes
every record and changes nothing.

If you kept the blob tree, put it back at `TOME_BLOB_ROOT` before starting the
worker. If you did not, the archive still restores: articles, bodies, and reading
state are all in the file, and the images are gone unless the worker can fetch them
again from sites that still exist.

## What survives exactly, and what does not

Verified by exporting a 385-article archive, restoring it into an empty database,
and comparing every row:

- **Every stored body came back byte for byte** — all 341 of them.
- Metadata, reading state, tags and highlights: identical.
- A fetched body comes back fetched, so re-extraction can still improve it later. An
  imported body comes back immutable, because it may be the only surviving copy of a
  page that is gone.
- An article imported from Wallabag is still recorded as that article, so
  re-importing your Wallabag library afterwards recognizes it rather than adding it
  twice.

One measured difference: the *derived* plain text is recomputed from the body on the
way in rather than carried, so where two block elements abut it can lose a word
boundary — `service.Data` instead of `service. Data`. In that archive it affected 16
of 341 bodies and 46 words in total. It changes nothing you read; it can very
occasionally affect what a search matches.

## Doing it on a schedule

The export is a single command with no state, so it composes:

```sh
tome export | gzip > "archive-$(date +%F).json.gz"
```

On Kubernetes, the manifests already run a nightly `pg_dump`
([back up and restore](back-up-and-restore.md) when that page exists). A `pg_dump`
is the better backup of the database — it restores the schema, the job queue and
everything else — and this export is the better *portable* copy: readable, and
readable by something other than PostgreSQL.

Keep both if the archive matters. They fail differently.
