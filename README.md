<h1 align="center">
  <img src="docs/assets/logo.png" alt="Tomekeeper" width="520">
</h1>

<p align="center">
  A self-hosted feed aggregator that permanently archives what it reads.
</p>

Feed readers treat articles as transient, and most feeds are truncated, so what
you keep is a headline and two sentences. Read-later apps archive well but
subscribe poorly. Tomekeeper does both, and the archive is the point: every
item it ingests becomes an offline-readable article with its images, stored as
files on disk that open in a browser with the service stopped and the database
gone.

**Status: early, and usable.** It polls feeds, fetches the linked pages
politely, extracts readable bodies, localizes the images, and writes each
article as a standalone page — open `index.html` from a file manager with this
service stopped and the database gone, and the article renders with its images.

What is there:

- **Reading.** Unread stream, article reader, full-text search across the whole
  archive, browsing by category, starred and a manual reading list. Unread and
  Everything narrow to one category, so "what is new in the comics" is a list of its
  own. Any list can be marked read in one go — scoped to that list, and it tells you
  how many before it does it. Keyboard-driven throughout; no build step, no CDN, no
  JavaScript required for anything but the shortcuts.
- **Saving by hand.** Paste a URL and it is fetched, extracted and archived like
  anything a feed brought.
- **Taking it away again.** A download on the settings page, or `tome export` —
  the whole archive as one file that
  `tome import` reads back — verified by restoring into an empty database and
  comparing, with every stored body identical. No lock-in beyond the pictures, which
  are files on disk you already have.
- **Bringing a library with you.** `tome import`, or an upload on the Saved page,
  reads a Wallabag JSON export:
  deduplicated against what your feeds already carried, dated when you saved each
  page rather than when you imported it, safe to re-run, and it reports what it
  will do before it does it — including the pages your old reader never actually
  held, which it queues to fetch for itself.
- **Installable.** Add it to a home screen and it draws its own navigation — a tab
  bar, a way back from every article, previous/next, a reload control, and the
  unread count in the title and on the app icon — because a standalone window
  provides none of that.
- **Subscriptions.** Add a feed by pasting a site's address — it finds the feed the
  page advertises, tests it, and shows you what it carries before you subscribe.
  OPML import from the CLI or by upload. Feed health with the last error each feed
  gave, a queue of anything that did not come through cleanly, and a button to check
  every feed now rather than waiting for the adaptive interval. The list sorts and
  filters by any column, and **Edit** on a row corrects an address, re-files a feed,
  stops polling it, or unsubscribes — a moved feed keeps its history instead of
  becoming a second subscription, and unsubscribing deletes the subscription without
  deleting anything it archived.
- **How often it looks.** Intervals are learned per feed, which is right until it is
  not: a general cadence in **Settings** and an override on any one feed's **Edit**
  form take it over, and the feed's own setting wins. Neither can poll more often than
  the instance's floor — that promise belongs to the servers being polled, not to the
  reader — and asking for less often than the ceiling is never refused.
- **Fixing a site that extracts badly.** Domain rules are a page rather than a
  shell command: write the selector, and reprocess that domain from the same screen.
  The failed-fetch queue links to the form for the host that failed.
- **More than one copy of a page.** An imported body and a freshly extracted one can
  coexist; the article page shows what each holds and lets you pick. Nothing
  automatic overrules an import, because it may be the last copy there is.
- **Keeping, and letting go.** Star or keep an article to protect its stored copy
  forever; optional retention releases the bodies of things you have read and did
  not keep. Off by default.
- **Looking at it for hours.** Six palettes plus the neutral one it starts with,
  each following your system light/dark preference or pinned to one, chosen from a
  settings page. Optionally, articles mark themselves read as you scroll past them
  on the unread lists — off by default, and never for anything starred or saved.
- **Running it.** Kubernetes manifests that stand up the service, its worker, its
  PostgreSQL, a migration Job and a nightly backup CronJob, plus Prometheus metrics
  on their own port — deliberately not the port the reader is on.

- **Reading it on a phone, in an app you already have.** It speaks the Fever sync
  protocol, so most third-party RSS clients can read the archive — and what they get
  in the body is the *extracted article*, not the two-sentence summary the feed
  shipped. Read and starred state syncs both ways; archived images travel with it.

- **The handful of sites that need a browser.** Some pages send an empty shell and
  build the article in JavaScript. Flag the domain and a headless browser fetches it
  instead — off by default, scaled to zero, and it refuses to load images, fonts and
  media while it does so.

**Not built yet:** importing from read-later tools other than Wallabag, and more than
one user account.

## Quick start

With Docker Compose, which builds nothing:

```sh
curl -O https://raw.githubusercontent.com/runlevel-six/tomekeeper/master/compose.yaml
curl -O https://raw.githubusercontent.com/runlevel-six/tomekeeper/master/.env.example
cp .env.example .env && $EDITOR .env     # a password, and a contact URL
docker compose up -d
```

Then sign in at <http://localhost:8080>. See
[Install with Docker Compose](docs/how-to/install-docker-compose.md) for the parts
worth knowing — the worker's memory limit, the cookie setting, and where the archive
actually lives.

Locally, from source:

```sh
docker run -d --name tome-db -e POSTGRES_USER=tome -e POSTGRES_PASSWORD=tome \
  -e POSTGRES_DB=tome -p 5432:5432 postgres:16-alpine

git clone https://github.com/runlevel-six/tomekeeper.git
cd tomekeeper
task build

export TOME_DATABASE_URL='postgres://tome:tome@localhost:5432/tome?sslmode=disable'
export TOME_BLOB_ROOT="$PWD/archive"
export TOME_CONTACT_URL='https://example.com/about'   # be contactable
export TOME_PASSWORD='choose-something'               # read only by migrate

./bin/tome migrate                        # create the schema, seed the user
./bin/tome import-opml subscriptions.opml # your OPML from any other reader
./bin/tome worker                         # poll, fetch, extract, localize images

# Then open any archived article straight from disk:
find "$TOME_BLOB_ROOT/articles" -name index.html | head -1
```

```console
$ ./bin/tome serve &
$ curl -s localhost:8080/readyz
{"status":"ready","checks":{"database":"ok","schema":"ok"}}
```

Then sign in at <http://localhost:8080>. The full walkthrough is
[Tutorial 1](docs/tutorials/01-first-run.md).

On Kubernetes, where the manifests bring their own PostgreSQL:

```sh
cp -r deploy/overlays/example deploy/overlays/local
$EDITOR deploy/overlays/local/kustomization.yaml   # hostname, storage class, image

kubectl create namespace tomekeeper
kubectl -n tomekeeper create secret generic tomekeeper \
  --from-literal=password="$(openssl rand -base64 24)" \
  --from-literal=postgres-password="$(openssl rand -base64 24)" \
  --from-literal=session-key="$(openssl rand -base64 32)"
kubectl apply -k deploy/overlays/local
```

`deploy/overlays/local` is gitignored on purpose — a hostname and a storage class
are yours, not the project's. See
[Install on Kubernetes](docs/how-to/install-kubernetes.md) for the handful of
things that will confuse you once.

## Releases

Releases are git tags, and the container image carries the same string:

```
ghcr.io/runlevel-six/tomekeeper:v0.7.0
```

That is the tag `v0.7.0`, it is what `tome version` reports inside the image, and it
is what `deploy/base/kustomization.yaml` pins. One identifier in git, in the
registry, in a Deployment, and in a log line — and CI refuses to publish a version
it has published before, so a version number always means one set of bytes.

| Tag | Points at |
|---|---|
| `vX.Y.Z` | one release, forever — what a deployment should use |
| `latest` | the newest release |
| `edge` | the tip of the default branch, not a release |
| `sha-<commit>` | one build, forever |

While the major version is `0`: a **patch** release is fixes only and never
migrates the database, so upgrading is a tag change and nothing else; a **minor**
release may add a migration or change a default. [CHANGELOG.md](CHANGELOG.md) says
which, and [Cut a release](docs/how-to/cut-a-release.md) is the process.

## Documentation

[**docs/**](docs/index.md) — configuration, CLI, and design rationale, in
[Diátaxis](https://diataxis.fr) form.

## Development

```sh
task                  # list tasks
task check            # format, vet, lint, test, build — everything CI runs
task test             # unit tests; integration tests skip without a database
task test:integration # all tests, failing if TOME_TEST_DATABASE_URL is unset
task dco              # verify new commits carry a DCO sign-off
task fuzz             # fuzz URL canonicalization
task docker:smoke     # acceptance criteria against the container image
```

Requires Go 1.26+ and [Task](https://taskfile.dev). Docker is needed for the
image tasks and for a PostgreSQL to run the integration tests against:

```sh
docker run -d --name tome-test -e POSTGRES_USER=tome -e POSTGRES_PASSWORD=tome \
  -e POSTGRES_DB=tome_test -p 5432:5432 postgres:16-alpine
export TOME_TEST_DATABASE_URL='postgres://tome:tome@localhost:5432/tome_test?sslmode=disable'
```

## Support

This is a personal project, published because it may be useful, not because it
is a product.

- **Supported:** it works for the maintainer's setup, and bug reports with
  enough detail to reproduce are welcome.
- **Not supported:** your deployment, your Postgres, your ingress. PRs are
  welcome and will be read; I will not debug an environment I cannot see.

The most useful thing you can send is a page this fails to extract. See
[CONTRIBUTING.md](CONTRIBUTING.md) for what to include, and for the two rules
that matter before changing anything.

There is no roadmap commitment and no release cadence.

## License

[GNU AGPL-3.0](LICENSE). Use it, fork it, run it. If you run a modified version
as a network service, the license requires you to offer those modifications to
its users.
