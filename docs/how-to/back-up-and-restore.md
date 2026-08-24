# Back up and restore

The archive is two things, and a backup that covers one of them is not a backup:

| | What it holds |
|---|---|
| **The database** | Articles, bodies, feeds, reading state, tags, highlights, import records |
| **The blob root** | Every original fetched page and localized image, plus each article's standalone `index.html` and `meta.json` |

Losing either alone is survivable and neither loss is silent. A database restored
without its files renders text and blank frames; files without a database are a
directory of readable HTML pages and no reader.

**`tome backup` covers both halves in one file**, and can prove that file is whole.
It is a command in the binary rather than a recipe per platform, because an archive
can be running under Kubernetes, under Compose or from a systemd unit, and because
the image is distroless — no shell, no `tar` — so every recipe necessarily ran outside
the application in whatever the platform happened to provide.

## Take one

```sh
tome backup --to /backups
```

```
wrote /backups/tome-1.tar
383.8 MB, schema 22: 26178 rows across 15 tables, and 10774 files
check it with: tome backup --verify /backups/tome-1.tar
```

**`--to` naming a directory means "you choose the name"**, and the name rotates by day
of the week — `tome-1.tar` through `tome-7.tar`, the convention the old database dump
used. That is what lets a scheduled job rotate without a shell to run `date` in. Name
a file instead and it writes exactly that, refusing to replace one that exists unless
`--force` says so. With no `--to` at all it streams to standard output, which is what
you want when piping it somewhere:

```sh
tome backup | restic backup --stdin --stdin-filename tomekeeper.tar
```

Either way it writes through a `.partial` file and moves it into place, so an
interrupted backup never leaves something that looks finished.

## Check that it is whole

```sh
tome backup --verify /backups/tome-1.tar
```

```
383.8 MB, written by tome v1.0.1 at 2026-08-24 03:17 UTC, schema 22
10789 entries verified against the hashes the archive recorded
this archive is whole
```

**This needs no database and no configuration** — only the file and the binary. That
is deliberate: "is this backup any good" has to be answerable on whatever machine the
file ended up on, months later.

The manifest records the SHA-256 of every entry as it went into the archive — every
file and every table — so verification compares what arrived against what was read.
Nothing is skipped and nothing is taken on trust.

**It does not compare against the database's hashes, and v1.0.0 tried to.**
`articles.raw_blob_sha` is the hash of a page as it was *fetched* while the file on disk
is that page gzipped, and `assets.sha256` identifies a downloaded image while the file
is a transcode of it. Comparing a stored file against either is meaningless: on the
first real archive it was pointed at, 5,790 of 6,208 healthy files were reported as
corrupt. Fixed in v1.0.1, which is also why an archive written by v1.0.0 refuses to
verify rather than lying about itself — take a fresh one.

Two failures it names precisely, because both have happened:

- **A short copy.** A documented `kubectl run --rm -i` streaming a tar once delivered
  73.9 MB of 307 MB, 37 files of 8,946, and exited 0. `--verify` reports the entries the
  manifest names and the archive does not have.
- **A file the archive itself had already lost.** If a prune or an expiry runs while a
  backup is being taken, the database can reference a file the tree no longer holds. The
  manifest records those separately — this is what the database's hashes are still read
  for — so a faithful copy of an archive that is short a file still verifies, while
  saying which rows will restore without their bytes. A bad copy and an incomplete
  archive are different faults and must not be confused.

## Schedule it

Same command everywhere. Only the way it is scheduled differs.

### Kubernetes

Nothing to do: the manifests run it nightly at 3:17, into the `tomekeeper-backups`
volume, rotating by day of the week. It runs the application's own image and mounts the
blob volume **read-only** — a backup has no business writing into what it is copying.

```sh
kubectl -n tomekeeper get cronjob tomekeeper-backup
kubectl -n tomekeeper logs job/tomekeeper-backup-<id>
```

Seven rotating archives now hold the whole archive rather than just the database, so
the volume is sized 20Gi in the manifests. **Do the arithmetic for your own archive**
before assuming that is enough: it scales with the images, which are the majority of
the bytes.

### Docker Compose

A profile, so it runs when asked rather than idling:

```sh
docker compose run --rm backup
docker compose run --rm backup backup --verify /backups/tome-1.tar
```

`./backups` is a bind mount rather than a named volume, on purpose: a backup you cannot
find in your own filesystem is one you will not copy off the machine, and copying it off
is the entire point. Put that command in a host cron entry:

```cron
17 3 * * *  cd /srv/tomekeeper && docker compose run --rm backup >> /var/log/tome-backup.log 2>&1
```

### A single machine, systemd

```ini
# /etc/systemd/system/tome-backup.service
[Service]
Type=oneshot
User=tome
EnvironmentFile=/etc/tomekeeper/env
ExecStart=/usr/local/bin/tome backup --to /var/backups/tomekeeper
ExecStartPost=/usr/local/bin/tome backup --verify /var/backups/tomekeeper/tome-%%u.tar
```

```ini
# /etc/systemd/system/tome-backup.timer
[Timer]
OnCalendar=*-*-* 03:17:00
Persistent=true

[Install]
WantedBy=timers.target
```

`ExecStartPost` is the part worth copying: a scheduled backup that is never verified is
a scheduled hope.

## Get it off the machine

**Nothing here does that, and that is a deliberate limit rather than an omission.** One
copy of an archive on the same disk as the archive protects you from a mistake, not from
the disk. Where the second copy lives is a decision about somebody's own network and
budget, and the tools that do it well already exist:

```sh
restic -r s3:… backup /backups/tome-1.tar
rclone copy /backups/ remote:tomekeeper-backups/
```

Or pipe the backup straight into one, as above, and never land it locally at all.

## Restore

**Stop the writers first.** A restore replaces every table and rewrites the tree, so a
worker still fetching into them will fight it. This is also why restoring is a command
and never a button in the web interface: nothing inside the application can arrange for
the application to be stopped.

```sh
# Kubernetes
kubectl -n tomekeeper scale deploy/tomekeeper-server deploy/tomekeeper-worker --replicas=0
kubectl -n tomekeeper exec -i deploy/... # see Install on Kubernetes for reaching a pod
                                         # with the volume mounted

# Compose, or a single machine
tome restore /backups/tome-1.tar
```

```
restored 26178 rows and 10774 files (340.0 MB) from an archive taken 2026-08-24 03:17 UTC at schema 22
the job queue was not restored: the schedulers rebuild it within a minute of `tome worker` starting
open an article with images before calling this done — that is the check the database alone cannot give you
```

What it does, in order: migrates the schema, replaces every table it carries in one
transaction, resets the id sequences, then unpacks the tree.

Three refusals, all of them before anything has changed:

- **A database that already holds articles**, unless `--force`. Restoring over a live
  archive is the one mistake here that cannot be undone, and the likeliest reason to be
  here with a populated database is a mistyped argument.
- **An archive taken at a schema this build cannot reach.** Restore forward, never back.
- **An archive written by a newer format than this build understands.**

**The job queue is deliberately not restored.** A `pg_dump` carries River's tables
along, so a restored archive inherits the queue as it stood when the dump was taken and
the worker starts chasing articles that may since have been pruned. The schedulers
rebuild what is outstanding within a minute of starting, which is better than
resurrecting it.

Then start the writers and **open an article with images**. That is the check nothing
else gives you: images are served from the blob root, so a database restored without its
files looks entirely healthy from every command and renders blank frames to a reader.

## The other kind of copy

`pg_dump` still works and nothing here prevents it. If you want a database backup in a
format your own tooling already understands, take one — it is the smaller half, and
`pg_restore` can pull a single table out of it, which this format cannot.

And `tome export` is a different thing again: one reader's articles as JSON, portable
into another system and readable by `tome import`. A backup is the household's bytes,
restorable onto a new machine, and only this software can read it. See
[Export everything](export-everything.md).

## See also

- [Install on Kubernetes](install-kubernetes.md#backups) — reaching a pod with the
  volume mounted, and the scale-down sequence
- [CLI](../reference/cli.md#tome-backup) — every flag, and what the manifest records
- [Choose what is forgotten](choose-what-is-forgotten.md) — retention, and `tome prune`,
  which are how an archive gets *smaller* on purpose
