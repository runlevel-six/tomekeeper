# Tomekeeper

A self-hosted feed aggregator that permanently archives what it reads.

Feed readers treat articles as transient, and most feeds are truncated, so what
you keep is a headline and two sentences. Read-later apps archive well but
subscribe poorly. Tomekeeper does both, and the archive is the point: every
item it ingests becomes an offline-readable article with its images, stored as
files on disk that open in a browser with the service stopped and the database
gone.

**Status: early. Milestone 4 of 9.** It polls feeds, fetches the linked pages
politely, extracts readable bodies, localizes the images, and writes each
article as a standalone page — open `index.html` from a file manager with this
service stopped and the database gone, and the article renders with its images.
It also serves a web interface to read in: an unread stream, a reader, full-text
search across the archive, feed health, and a queue of anything that did not come
through cleanly. Keyboard-driven, no build step, no CDN.

## Quick start

```sh
docker run -d --name tome-db -e POSTGRES_USER=tome -e POSTGRES_PASSWORD=tome \
  -e POSTGRES_DB=tome -p 5432:5432 postgres:16-alpine

git clone https://github.com/runlevel-six/tomekeeper.git
cd tomekeeper
task build

export TOME_DATABASE_URL='postgres://tome:tome@localhost:5432/tome?sslmode=disable'

export TOME_BLOB_ROOT="$PWD/archive"
export TOME_CONTACT_URL='https://example.com/about'   # be contactable

./bin/tome migrate                        # create the schema
./bin/tome import-opml subscriptions.opml # your OPML from any other reader
./bin/tome worker                         # poll, fetch, extract, localize images

# Then open any archived article straight from disk:
find "$TOME_BLOB_ROOT/articles" -name index.html | head -1
```

```console
$ ./bin/tome serve &
$ curl -s localhost:8080/readyz
{"status":"ready","checks":{"database":"ok"}}
```

The full walkthrough is [Tutorial 1](docs/tutorials/01-first-run.md).

## Documentation

[**docs/**](docs/index.md) — configuration, CLI, and design rationale, in
[Diátaxis](https://diataxis.fr) form.

## Development

```sh
task                  # list tasks
task check            # format, vet, lint, test, build — everything CI runs
task test             # unit tests; integration tests skip without a database
task test:integration # all tests, failing if TOME_TEST_DATABASE_URL is unset
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

There is no roadmap commitment and no release cadence.

## License

[GNU AGPL-3.0](LICENSE). Use it, fork it, run it. If you run a modified version
as a network service, the license requires you to offer those modifications to
its users.
