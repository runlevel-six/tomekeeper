# Back up and restore

The archive is two things, and a backup that covers one of them is not a backup:

| | What it holds | How it is backed up |
|---|---|---|
| **The database** | Articles, bodies, feeds, reading state, tags, highlights, import records | `pg_dump` — automatic on Kubernetes |
| **The blob root** | Every original fetched page and localized image, plus each article's standalone `index.html` and `meta.json` | A file copy — **not** automatic, anywhere |

The database is the smaller and the more irreplaceable: subscriptions and reading
state exist nowhere else. The blob root is the larger, and the one you could
reconstruct a reading experience from with no software at all — which is why it is
stored as ordinary files in ordinary directories.

Losing either alone is survivable and neither loss is silent. A database restored
without its files renders text and blank frames; files without a database are a
directory of readable HTML pages and no reader.

## On Kubernetes

The manifests run a nightly `pg_dump` — 3:17am, custom format, seven files on
rotation named for the day of the week, written to `.partial` and moved into place so
an interrupted dump never looks like a finished one. The volume is 5Gi, which a large
archive's dumps can eventually reach; the symptom is a failing CronJob rather than
anything a reader notices.

**[Install on Kubernetes](install-kubernetes.md#backups) has the procedure** —
getting a dump off the backups volume, scaling the writers down before restoring, and
the exact `pg_restore` invocation. It is not repeated here, because a second copy of
an operational procedure is a second copy to keep correct.

What that page does not cover is copying the blob tree, and the obvious approach does
not work: **the tomekeeper image is distroless**, so it has no shell and no `tar`,
which means `kubectl exec … tar` fails and so does `kubectl cp`, which is `tar`
underneath. Mount the volume somewhere that has them:

```sh
kubectl -n tomekeeper run blob-copy --rm -i --restart=Never \
  --image=busybox:1.36 \
  --overrides='{"spec":{"containers":[{"name":"blob-copy","image":"busybox:1.36",
    "command":["tar","-C","/blobs","-cf","-","."],"stdin":true,
    "volumeMounts":[{"name":"blobs","mountPath":"/blobs","readOnly":true}]}],
    "volumes":[{"name":"blobs","persistentVolumeClaim":{"claimName":"tomekeeper-blobs"}}]}}' \
  > tome-blobs-$(date +%F).tar
```

Read-only on the mount, because a backup has no business being able to write to what
it is copying. The claim is `ReadWriteOnce`, so this pod has to land on the node
already running the worker — automatic on one node, and the same co-location
constraint the deployment already carries on more.

A volume snapshot, if the storage class supports one, is faster and skips the copy
entirely.

## Anywhere else

Both halves are ordinary commands:

```sh
pg_dump "$TOME_DATABASE_URL" -Fc -f tome-$(date +%F).dump
tar -C "$TOME_BLOB_ROOT" -cf tome-blobs-$(date +%F).tar .
```

To restore, **stop the writers first** so the restore is not racing the worker, then:

```sh
createdb tome
pg_restore -d tome --no-owner --clean --if-exists tome-2026-08-18.dump
tar -C "$TOME_BLOB_ROOT" -xf tome-blobs-2026-08-18.tar

tome migrate    # brings the schema forward if the dump predates this build
tome serve
```

`tome migrate` after a restore is neither optional nor harmful. The dump carries
whatever schema version it was taken at, and a newer build needs its migrations
applied before it will serve — `serve` fails readiness on a mismatch rather than
starting and breaking later, so getting this wrong is loud rather than subtle.

## Check that it worked

A restore nobody has read from is not a restore:

```sh
tome archive stats
curl -s localhost:8080/readyz
```

Then **open an article with images in the reader.** That is the check the database
alone cannot give you: images are served from the blob root, so a database restored
without its files looks entirely healthy from every command above and renders blank
frames to a reader.

## The portable copy

`pg_dump` is the better backup — it restores the schema, the job queue and everything
else exactly. It is also a PostgreSQL-specific binary format that only `pg_restore`
reads, and only for as long as a compatible PostgreSQL exists.

For a copy that outlives all of that, there is [export everything](export-everything.md):
one JSON file holding every article, its metadata, your reading state, tags and
highlights, which `tome import` reads back into an empty database and which a text
editor can open. Verified by restoring a real archive into an empty database and
comparing — every stored body came back byte for byte.

Keep both if the archive matters. They fail differently: a dump is exact and brittle,
an export is readable and lossy at the edges.

## What is not done, and is worth knowing

**No restore drill runs anywhere.** The plan's testing strategy asks for one — restore
a dump into an empty database in CI and assert the service starts and serves — on the
grounds that a backup nobody has restored is not a backup. The nightly dump runs and
has never been read back.

Until that exists, the check above is manual, and doing it deliberately once is worth
more than the schedule that produces the files.
