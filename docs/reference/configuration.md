# Configuration

Tomekeeper is configured entirely by environment variables. There is no
configuration file and no command-line equivalent for any of these values.

Every variable name begins with `TOME_`. Configuration is read and validated
once, at startup, before any other work. An invalid or incomplete configuration
prevents the process from starting; it is never deferred to first use.

## Variables

| Variable | Type | Default | Required | Description |
|---|---|---|---|---|
| `TOME_DATABASE_URL` | URL | — | yes | PostgreSQL connection URL. Scheme must be `postgres` or `postgresql`, and a host must be present. |
| `TOME_HTTP_ADDR` | host:port | `:8080` | no | Listen address for `tome serve`. A port is required; the host may be empty to listen on all interfaces. |
| `TOME_LOG_LEVEL` | enum | `info` | no | Minimum level emitted. One of `debug`, `info`, `warn`, `error`. Case-insensitive. |
| `TOME_LOG_FORMAT` | enum | `json` | no | Log output format. One of `json`, `text`. |
| `TOME_SHUTDOWN_TIMEOUT` | duration | `15s` | no | Time allowed for in-flight requests to finish after a termination signal. Must be positive. Go duration syntax (`30s`, `1m`, `1m30s`). |
| `TOME_USERNAME` | string | `tome` | no | The single v1 user, created by `tome migrate`. Changing it renames the existing user rather than creating a second one. |
| `TOME_CONTACT_URL` | URL | — | no | Published in the outbound `User-Agent` as `tomekeeper/<version> (+<url>)`. Must be absolute if set. Strongly encouraged before pointing this at anyone else's server: it is how an operator finds out who to ask when they want it to stop. |
| `TOME_POLL_MIN_INTERVAL` | duration | `15m` | no | Floor for the adaptive poll interval. No feed is polled more often. |
| `TOME_POLL_MAX_INTERVAL` | duration | `24h` | no | Ceiling for the adaptive poll interval. Must be at least the floor. |
| `TOME_FEED_FAILURE_THRESHOLD` | int | `20` | no | Consecutive failures after which a feed is disabled. At least 1. A disabled feed keeps its last error and is surfaced, never silently dropped. |
| `TOME_WORKER_CONCURRENCY` | int | `5` | no | Jobs `tome worker` runs at once. At least 1. |

### Value handling

- A variable set to the empty string, or to whitespace only, is treated as
  unset and takes its default. `TOME_DATABASE_URL` set to `""` is therefore a
  missing required value, not an empty one.
- Values are not expanded, interpolated, or read from files. `TOME_X_FILE`
  conventions are not supported.

### Validation

All problems are reported together, on stderr, in a single run:

```
tome: invalid configuration:
  - TOME_DATABASE_URL is required: a PostgreSQL connection URL, for example postgres://tome:password@localhost:5432/tome?sslmode=disable
  - TOME_LOG_FORMAT "logfmt" is not valid, want one of: json, text

See docs/reference/configuration.md for every setting.
```

The process then exits with code `2`. See [CLI](cli.md#exit-codes).

### Credential handling

`TOME_DATABASE_URL` normally contains a password. Any password in it is
replaced with `xxxxx` before the URL reaches a log line, including the
configuration summary logged at startup. The username, host, database name, and
query parameters are preserved, so the log still answers "is it pointed at the
right database".

## Example

```sh
export TOME_DATABASE_URL='postgres://tome:password@localhost:5432/tome?sslmode=disable'
export TOME_HTTP_ADDR='127.0.0.1:8080'
export TOME_LOG_LEVEL='debug'
export TOME_LOG_FORMAT='text'
export TOME_CONTACT_URL='https://example.com/about'

tome migrate
tome worker
```

## Which commands read what

All settings are read by every subcommand, because configuration is validated
as a whole. Only some affect a given command's behavior:

| Setting | `serve` | `worker` | `migrate` | `import-opml` |
|---|---|---|---|---|
| `TOME_DATABASE_URL` | yes | yes | yes | yes (not for `--dry-run`) |
| `TOME_HTTP_ADDR`, `TOME_SHUTDOWN_TIMEOUT` | yes | — | — | — |
| `TOME_LOG_LEVEL`, `TOME_LOG_FORMAT` | yes | yes | yes | yes |
| `TOME_USERNAME` | — | — | creates the user | selects the user |
| `TOME_CONTACT_URL` | — | yes | — | — |
| `TOME_POLL_*`, `TOME_FEED_FAILURE_THRESHOLD`, `TOME_WORKER_CONCURRENCY` | — | yes | — | — |

## Not yet implemented

The following are described in the implementation plan but are not read by any
current code, and setting them has no effect:

| Variable | Arrives with |
|---|---|
| `TOME_BLOB_ROOT` — filesystem root of the archive | M3 |

This table exists so that a variable seen in an issue, a manifest, or the plan
can be identified as "not wired up yet" rather than "misspelled".
