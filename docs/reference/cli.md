# CLI

Tomekeeper is a single binary, `tome`, with subcommands. Every subcommand reads
its settings from the environment; see [Configuration](configuration.md).

```
tome <subcommand>
```

Running `tome` with no subcommand prints usage to stderr and exits `2`.

## Subcommands

### `tome serve`

Runs the HTTP server in the foreground until it receives `SIGINT` or `SIGTERM`.

Takes no flags or positional arguments. Passing any argument is an error and
exits `2`; this is deliberate, so that a flag someone expects to exist fails
visibly instead of being ignored.

On a termination signal the server stops accepting new connections and waits up
to `TOME_SHUTDOWN_TIMEOUT` for in-flight requests to complete. A clean shutdown
exits `0`. If the timeout expires with requests still in flight, the failure is
logged at `error` and the process exits `1`.

**Required configuration:** `TOME_DATABASE_URL`.

### `tome version`

Prints the build identity to stdout and exits `0`.

```
tomekeeper v0.1.0 (a1b2c3d) built 2026-08-16T23:00:03Z go1.26.5 linux/amd64
```

Version, commit, and build date are injected at link time. When they are not —
a `go build` with no flags, or `go run` — they are recovered from the Go build
info embedded from the git work tree, and a build with uncommitted changes is
reported with a `-dirty` suffix.

### `tome help`

Prints usage to stdout and exits `0`. `tome -h` and `tome --help` are
equivalent.

## HTTP endpoints

Served by `tome serve`. Both respond to `GET` and `HEAD`; any other method
returns `405`. Both set `Cache-Control: no-store` and return
`application/json; charset=utf-8`.

### `GET /healthz` — liveness

Always returns `200` while the process is able to serve HTTP.

```json
{"status": "ok"}
```

It deliberately does not check the database or any other dependency. A liveness
failure causes the container to be killed, and killing every replica because a
dependency is briefly unreachable converts a recoverable outage into a crash
loop. Dependency state belongs to `/readyz`.

### `GET /readyz` — readiness

Returns `200` when every registered dependency check passes, `503` when any
fails. The whole probe is bounded at 3 seconds.

```json
{"status": "ready", "checks": {"database": "ok"}}
```

```json
{"status": "not ready", "checks": {"database": "connection refused"}}
```

The `checks` field is omitted when no checks are registered. **At M0 no checks
are registered**, so `/readyz` reports ready as soon as the process is serving.
The database check arrives with M1, the blob root check with M3.

Requests to both endpoints are logged at `debug` rather than `info`, so that
orchestrator probes do not dominate the log.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success, including a clean shutdown after a termination signal. |
| `1` | Runtime failure. The server could not bind its address, or shutdown exceeded its timeout. |
| `2` | Bad invocation or invalid configuration. Nothing was started. |

The distinction between `1` and `2` is part of the operational contract: a
supervisor can tell "you configured me wrongly" from "something broke" without
parsing log output. Configuration errors are written to stderr as plain text,
not through the structured logger, because the logger's own configuration is
among the things that may have just failed to validate.

## Not yet implemented

| Subcommand | Arrives with |
|---|---|
| `tome worker` — job worker pool | M1 |
| `tome migrate` — apply database migrations | M1 |
| `tome reextract` — re-run extraction over the archive | M2 |
| `tome import` / `tome export` | M6 |
| `tome reindex` — rebuild the search index | M4 |

These are listed in the implementation plan and are not yet accepted by the
binary; invoking one exits `2` as an unknown subcommand.
