# Tomekeeper

A self-hosted feed aggregator that permanently archives what it reads.

Feed readers treat articles as transient, and most feeds are truncated, so what
you keep is a headline and two sentences. Read-later apps archive well but
subscribe poorly. Tomekeeper does both, and the archive is the point: every
item it ingests becomes an offline-readable article with its images, stored as
files on disk that open in a browser with the service stopped and the database
gone.

**Status: early. Milestone 0 of 9.** The binary starts, validates its
configuration, logs, and answers health probes. It does not yet poll feeds,
fetch pages, or store anything. There is nothing here to use yet.

## Quick start

```sh
git clone https://github.com/runlevel-six/tomekeeper.git
cd tomekeeper
task build

export TOME_DATABASE_URL='postgres://tome:password@localhost:5432/tome?sslmode=disable'
./bin/tome serve
```

```console
$ curl -s localhost:8080/healthz
{"status":"ok"}
```

Or with the container image:

```sh
task docker:build
docker run --rm -p 8080:8080 \
  -e TOME_DATABASE_URL='postgres://tome:password@db:5432/tome' \
  tomekeeper:latest
```

## Documentation

[**docs/**](docs/index.md) — configuration, CLI, and design rationale, in
[Diátaxis](https://diataxis.fr) form.

## Development

```sh
task            # list tasks
task check      # format, vet, lint, test, build — everything CI runs
task test       # tests with the race detector
task docker:smoke   # acceptance criteria against the container image
```

Requires Go 1.26+ and [Task](https://taskfile.dev). Docker is needed only for
the image tasks.

## Support

This is a personal project, published because it may be useful, not because it
is a product.

- **Supported:** it works for the maintainer's setup, and bug reports with
  enough detail to reproduce are welcome.
- **Not supported:** your deployment, your Postgres, your ingress. PRs are
  welcome and will be read; I will not debug an environment I cannot see.

There is no roadmap commitment and no release cadence.

## License

Not yet chosen. Until one is added, no license is granted.
