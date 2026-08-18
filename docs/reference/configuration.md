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
| `TOME_PASSWORD` | string | — | no | Password for the single user. Read **only by `tome migrate`**, which stores an argon2id hash and derives the Fever API key from it. `tome serve` never reads it. Unset leaves any existing password alone; unset on a first run means the web interface cannot be signed into. Setting it always rotates the Fever key, so mobile clients need reconnecting. |
| `TOME_SESSION_KEY` | string | — | no | Secret that session cookies are sealed with. Unset means one is generated at startup, so sessions work but do not survive a restart — `tome serve` warns when this happens. Generate with `openssl rand -base64 32`. Any length is accepted and stretched with HKDF, which does not manufacture entropy: a short secret is a weak secret. |
| `TOME_COOKIE_SECURE` | bool | `true` | no | Sets the `Secure` attribute on the session cookie, so it is only sent over HTTPS. Leave it on. Turn it off **only** when serving plain HTTP on a trusted network — browsers treat `localhost` as secure already, so a local first run does not need it. |
| `TOME_METRICS_ADDR` | host:port | `:9090` | no | Listen address for the Prometheus endpoint, served by both `serve` and `worker`. Empty disables it. **Deliberately not on the main HTTP port:** an Ingress routing `/` would publish it, and the outbound metrics name every host the archive fetches from. See [Metrics](metrics.md). |
| `TOME_RETAIN_AFTER_READ` | duration | unset | no | How long a **read** article keeps its stored body and images before they are released. Unset or `0` keeps everything forever, which is the default. Starred, kept, and manually saved articles are never expired at any setting. Values under `1h` are rejected, because a typo here deletes an archive. See [Retention](retention.md). |
| `TOME_CONTACT_URL` | URL | — | no | Published in the outbound `User-Agent` as `tomekeeper/<version> (+<url>)`. Must be absolute if set. Strongly encouraged before pointing this at anyone else's server: it is how an operator finds out who to ask when they want it to stop. |
| `TOME_POLL_MIN_INTERVAL` | duration | `15m` | no | Floor for the adaptive poll interval. No feed is polled more often. |
| `TOME_POLL_MAX_INTERVAL` | duration | `24h` | no | Ceiling for the adaptive poll interval. Must be at least the floor. |
| `TOME_FEED_FAILURE_THRESHOLD` | int | `20` | no | Consecutive failures after which a feed is disabled. At least 1. A disabled feed keeps its last error and is surfaced, never silently dropped. |
| `TOME_WORKER_CONCURRENCY` | int | `5` | no | Jobs `tome worker` runs at once. At least 1. |
| `TOME_FETCH_RPS` | float | `1` | no | Default per-host request rate. Fractional values are the useful ones: `0.5` is one request every two seconds. A domain rule's `--rate` overrides it for one host. |
| `TOME_FETCH_CONCURRENCY` | int | `10` | no | Outbound requests in flight across all hosts. Protects this machine; the per-host rate is what protects the sites. |
| `TOME_BLOB_ROOT` | path | `/var/lib/tomekeeper` | no | Filesystem root of the archive. Must be absolute — a relative path would resolve against whatever directory the process started in, and the archive would move between deployments. |

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

`TOME_PASSWORD` is never logged at all. It is absent from the configuration
summary by construction rather than by redaction — the summary lists its fields
explicitly, so a secret that is not in that list cannot be printed by any future
caller, however careless.

`TOME_SESSION_KEY` is likewise absent from the log. The summary reports only
whether one was configured, which is the operationally useful fact.

It is also deliberately scoped to one command. `tome serve` authenticates against
the stored hash and has no use for the cleartext, so the secret belongs to the
migration step alone. In Kubernetes that means the Secret is mounted on the
migration Job, not on the long-running Deployment.

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
| `TOME_PASSWORD` | — | — | sets the password | — |
| `TOME_SESSION_KEY`, `TOME_COOKIE_SECURE` | yes | — | — | — |
| `TOME_METRICS_ADDR` | yes | yes | — | — |
| `TOME_RETAIN_AFTER_READ` | — | yes | — | — |
| `TOME_CONTACT_URL` | — | yes | — | — |
| `TOME_POLL_*`, `TOME_FEED_FAILURE_THRESHOLD`, `TOME_WORKER_CONCURRENCY` | — | yes | — | — |
| `TOME_FETCH_RPS`, `TOME_FETCH_CONCURRENCY` | — | yes | — | — |
| `TOME_BLOB_ROOT` | — | yes | — | — |

## Storage

`TOME_BLOB_ROOT` must be writable by the worker and readable by the server, and
it must be on persistent storage — it holds the archive, and the database is an
index over it rather than the other way round.

It contains an article directory per article and a shared, content-addressed
tree of images:

```
<root>/articles/2026/08/the-article-slug-a1b2c3/index.html
<root>/articles/2026/08/the-article-slug-a1b2c3/meta.json
<root>/articles/2026/08/the-article-slug-a1b2c3/raw.html.gz
<root>/assets/sha256/a1/b2/a1b2c3….avif
```

See [Storage layout](storage-layout.md) for what each file is and how much it
costs. Size it with `tome archive stats` and `du -sh "$TOME_BLOB_ROOT"`.
