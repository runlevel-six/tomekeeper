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
underneath. Mount the volume somewhere that has them.

> **Do not stream the copy out of `kubectl run --rm -i`.** That is what this page
> recommended until 2026-08-20, and it silently produces a truncated archive.
> Measured against a 307 MB tree: **73.9 MB arrived — 24% — `tar -t` could list 37
> members before failing with "Skipping to next header", and `kubectl` exited 0.** A
> backup that reports success and holds a quarter of the archive is worse than no
> backup, because you will believe it. The cause is that `--rm -i` attaches to the
> pod's output and races the pod's own deletion; the stream is cut when the pod goes
> away.

Start a pod that stays up, then copy in a separate step:

```sh
kubectl -n tomekeeper apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: blob-reader
spec:
  # ReadWriteOnce, so this has to be the node already running the worker. On one
  # node that is automatic and this line can go; on more it is the difference
  # between a copy and a pod stuck in Pending with no explanation.
  nodeName: NODE-RUNNING-THE-WORKER
  restartPolicy: Never
  containers:
    - name: reader
      image: busybox:1.36
      command: ["sleep", "3600"]
      volumeMounts:
        - { name: blobs, mountPath: /blobs, readOnly: true }
  volumes:
    - name: blobs
      persistentVolumeClaim: { claimName: tomekeeper-blobs }
YAML

kubectl -n tomekeeper wait --for=condition=Ready pod/blob-reader

# The checksums first, so that "complete" is something you can check rather than
# something you hope.
kubectl -n tomekeeper exec blob-reader -- \
  sh -c 'cd /blobs && find . -type f | sort | xargs md5sum' > tome-blobs-$(date +%F).md5

kubectl -n tomekeeper exec blob-reader -- tar -C /blobs -cf - . > tome-blobs-$(date +%F).tar

kubectl -n tomekeeper delete pod blob-reader
```

Read-only on the mount, because a backup has no business being able to write to what
it is copying.

**Then check it, every time.** This is the step that makes the difference between the
two recipes visible:

```sh
mkdir -p check && tar -xf tome-blobs-2026-08-20.tar -C check
( cd check && find . -type f | sort | xargs md5sum ) | diff - tome-blobs-2026-08-20.md5 \
  && echo "every file, every checksum, identical"
```

A volume snapshot, if the storage class supports one, is faster and skips the copy
entirely — and skips this whole class of problem, because nothing streams through a
client.

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

`--no-owner` because a dump records the roles that owned each object and a fresh
database elsewhere has none of them. The Kubernetes procedure omits it deliberately:
it restores into the StatefulSet that the dump came from, where the `tome` role
already exists and ownership should be preserved. Restoring one of those dumps
anywhere else needs this flag, or the role created first.

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

## What a restore rewinds

Two things come back as they were at the moment of the dump, and neither is obvious
from the file:

**Your subscriptions.** A feed unsubscribed after the dump was taken is subscribed
again after the restore. Nothing archived is at risk — articles are the root entity and
survive their feeds — but the feed list is as of the backup, not as of the failure, so
re-check it rather than assuming it carried on from where you left off.

**The job queue.** River's tables are in the dump like any others, so the worker
inherits whatever was queued, retryable or in flight at dump time and starts working
through it. That is usually what you want after losing a database. The leader row comes
back too and is harmless: it carries an expiry and is taken over as soon as it lapses.

## What has been restored from, and what has not

**The database half was drilled on 2026-08-19, and it holds.** A dump the CronJob wrote
unattended — 21.4 MB, 03:17 — was copied off the backups volume and restored into an
empty PostgreSQL 16.14 with the invocation the
[Kubernetes procedure](install-kubernetes.md#backups) documents. It exited zero and said
nothing, which for `pg_restore` is the good outcome.

What came back was then checked rather than assumed: 1,856 articles, 74 feeds, 385
import records, the schema at migration 4, and `tome migrate` reporting nothing to do.
The article set was compared against the live database at the dump's own high-water
mark — the same count, and an identical MD5 over every canonical URL, so the same
articles rather than the same number of them. `tome serve` against the restored copy
answered `/readyz` with both checks healthy, paginated the reading lists, returned 247
matches for a full-text search, and rendered an article's body.

**The blob tree has now been restored from, on 2026-08-20, and both halves were
restored together.** Last night's unattended dump (`tome-4.dump`, 23.5 MB, written
03:17) and a complete copy of the 323.6 MB blob tree were taken off their volumes,
restored side by side into an empty PostgreSQL 16.14 and an empty directory, and served.

What was checked rather than assumed:

- **The blob copy is complete.** 8,946 files, and a checksum manifest taken at the
  source `diff`s clean against the same manifest taken after extraction — every file,
  every checksum, no difference. This is where the truncating recipe above was caught.
- **The database holds the same articles, not the same number of them.** 2,087 articles
  at the dump's own high-water mark, and an identical MD5 over every canonical URL in
  id order compared against the live database at the same cutoff.
- **Every file the database refers to is present.** All 3,244 rows in `assets` resolve
  to a file in the restored tree. None missing.
- **An article renders with its pictures.** `tome serve` against the restored pair
  answered `/readyz` with both checks healthy, signed in, and rendered an article
  carrying 94 archived images — all 94 served `200` with bytes identical to the files on
  disk. That is the specific thing the August drill could not do: it rendered the same
  page with empty frames, because the pictures were on the volume nobody had copied.
- **The archive still opens without the software.** 1,780 standalone `index.html` pages
  came back, 846 of them referencing archived images; across a 40-page sample every one
  of 181 relative image references resolves to a file on disk.

Two things the drill turned up that are worth knowing before you need them:

- **A dump can be older than the schema the running image needs.** This one was: it
  restored at migration 5 while the deployed binary needs 6, because the dump was
  written before that migration was applied. `tome migrate` against the restored copy
  fixed it in seconds — but restoring straight into a running cluster without that step
  reproduces the outage of 2026-08-20 exactly, with the server failing readiness and the
  worker crash-looping.
- **Getting the dump off its volume is a different recipe from getting the blob tree
  off**, and only the blob one was broken. The dump procedure opens an interactive
  shell rather than streaming, which is why it has always worked.

**No restore drill runs in CI**, which is what the plan's testing strategy actually
asks for — restore into an empty database and assert the service serves, on every
build. The drill above was done by hand; it retires the question of whether these files
are readable, it does not keep answering it.

**Nothing copies the blob tree automatically.** The recipe above is a recipe, run by a
person. The nightly CronJob covers the database only, and its output lands on a volume
on the same storage as the data it protects — which survives a bad `pg_restore` and does
not survive losing the pool. Off-cluster replication is deliberately out of scope for
now (the maintainer's call, 2026-08-20); when it is wanted, the tool is `restic` and the
question to answer first is where the bucket lives.

**The in-cluster restore sequence is rehearsed but not lived.** Scaling the writers to
zero, restoring through `kubectl exec` into the StatefulSet, and scaling back up was
exercised against a scratch namespace rather than against the archive, on the grounds
that practising on production is not a rehearsal.
