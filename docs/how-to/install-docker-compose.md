# Install with Docker Compose

You want Tomekeeper running on one machine — a home server, a NAS, a spare box —
without Kubernetes and without building anything.

This takes about five minutes, plus however long the first poll takes.

## What you need

Docker with Compose v2 (`docker compose version` prints something), and two files
from this repository: `compose.yaml` and `.env.example`. You do not need the rest of
the source; the image comes from the registry.

```sh
curl -O https://raw.githubusercontent.com/runlevel-six/tomekeeper/master/compose.yaml
curl -O https://raw.githubusercontent.com/runlevel-six/tomekeeper/master/.env.example
```

## 1. Settings

```sh
cp .env.example .env
$EDITOR .env
```

Three lines need a decision; everything else has a working default.

| | |
|---|---|
| `POSTGRES_PASSWORD` | The database's own password. Nothing outside these containers uses it. |
| `TOME_PASSWORD` | Your sign-in password. |
| `TOME_CONTACT_URL` | Goes in the `User-Agent` of every outbound request, so a site operator who wants this to stop can find out who to ask. A page of your own is enough. |

Worth setting before you get attached to being signed in:

```sh
echo "TOME_SESSION_KEY=$(openssl rand -base64 32)" >> .env
```

Without it a key is generated per process, so every restart signs you out.

## 2. Start it

```sh
docker compose up -d
```

Four services come up in order: PostgreSQL, then a one-shot migration that creates
the schema and your user, then the reader and the worker together.

```sh
docker compose ps
```

The migration shows as `exited (0)` — that is correct, it is not a service. The
server should be `healthy` within a few seconds; that status is real, because the
binary answers its own healthcheck rather than the container merely being up.

Then open <http://localhost:8080> and sign in with `tome` and the password you set.

## 3. Subscribe to something

Paste a site's address into **Feeds → Add a feed** and press **Test** — it follows
the page to whatever feed it advertises, so you do not have to hunt for the feed URL.
File it in a category and add it.

If you are coming from another reader, **Feeds → Import subscriptions** takes its
OPML export, and [Tutorial 2](../tutorials/02-import-your-feeds.md) walks the whole
move including a read-later library.

Then watch it work:

```sh
docker compose logs -f worker
```

Articles appear with their text first; images arrive over the following minutes or
hours, one polite request at a time.

## Things worth knowing

**The worker has a memory limit, and it is not arbitrary.** Image encoding costs
roughly 600MB per picture, and the figure barely moves with image size — the cost is
the call, not the pixels. `TOME_IMAGE_CONCURRENCY=1` and a 2GB limit are matched to
each other. Raising the concurrency without raising the limit is how the worker gets
killed part-way through an article, and the symptom is images that never finish.

**Cookies are not marked Secure here.** `.env.example` sets
`TOME_COOKIE_SECURE=false`, because a browser will not send a Secure cookie over
plain HTTP — you would appear to sign in and then silently not be. **Set it back to
`true` the moment there is TLS in front of this**, whether that is a reverse proxy,
a tunnel, or anything else.

**The archive is in Docker volumes.** `tomekeeper_blobs` holds every fetched page and
archived image; `tomekeeper_db-data` holds the database. Neither is backed up by
anything here — see [back up and restore](back-up-and-restore.md), which is worth
reading before the archive is large enough to miss.

**The compose file pins a release**, so `docker compose pull` fetches the same bytes
every time and an upgrade is a thing you choose. Set `TOME_IMAGE` in `.env` to move
it — `ghcr.io/runlevel-six/tomekeeper:v0.2.0` — or to `:latest` if you would rather
follow releases as they come. Do not point it at `:edge`, which is the tip of the
default branch and not a release at all.

## Upgrading

```sh
# In .env, or leave it alone to keep what the compose file pins.
TOME_IMAGE=ghcr.io/runlevel-six/tomekeeper:v0.2.0

docker compose pull
docker compose up -d
```

The migration runs again as part of that, which is correct and idempotent — the
`migrate` service is ordered before the others, so unlike the Kubernetes manifests
there is nothing here to get the wrong way round.

Read [the changelog](https://github.com/runlevel-six/tomekeeper/blob/master/CHANGELOG.md)
before a minor bump: a patch release (`v0.2.0` → `v0.2.1`) never migrates, so it is
only ever a pull and a restart.

## Stopping and removing

```sh
docker compose down            # stops everything, keeps the archive
docker compose down --volumes  # deletes the archive as well
```

The second one is not recoverable. Take a copy first — **Settings → Export**
downloads everything the database holds as a single file, and the images are in the
volume you are about to delete.
