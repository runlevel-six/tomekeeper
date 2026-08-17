# Dependencies

Every dependency needs a one-line justification here. This is not ceremony:
this is a single-maintainer project intended to hold a decade of reading, and
each dependency is something that can be abandoned, break compatibility, or
acquire a supply-chain problem while nobody is looking.

The bar is: *the standard library cannot do this, or doing it by hand would be
worse than the risk of the dependency.*

## Go module dependencies

Four libraries as of M1, across six modules — River publishes its driver and
type packages separately. Each is here because the standard library cannot do
the job and doing it by hand would be worse than the risk of the dependency.

| Dependency | Why |
|---|---|
| `github.com/jackc/pgx/v5` | The PostgreSQL driver. `database/sql` cannot use the binary protocol or the Postgres-specific types this schema relies on, and pgx is the maintained choice in Go. Its `stdlib` subpackage also gives goose the `*sql.DB` it wants without a second connection pool. |
| `github.com/pressly/goose/v3` | Schema migrations. Chosen over `golang-migrate` because it works as a *library* against an embedded `fs.FS`: the runtime image is distroless, with no shell and no filesystem to read `.sql` files from, so migrations have to travel inside the binary that expects them. |
| `github.com/riverqueue/river` | Postgres-backed job queue, with `riverdriver/riverpgxv5` and `rivertype`. Writing a correct queue — visibility timeouts, retries with backoff, unique jobs, periodic schedules — is a project in itself, and this one keeps the promise of a single stateful dependency. |
| `github.com/mmcdole/gofeed` | RSS, Atom, and JSON Feed parsing. The specifications are only half the problem; the other half is the decade of malformed real-world feeds this library already handles, which is a tax paid forever if written from scratch. |

Still done with the standard library, deliberately:

| What could have been imported | What is used instead |
|---|---|
| A CLI framework (`cobra`, `urfave/cli`) | A `switch` on `os.Args[1]` and one `flag.FlagSet` for the single command with a flag. |
| A config library (`viper`, `envconfig`) | ~150 lines in `internal/config`. Reflection-driven loaders make the error messages worse, and the error messages are the point of validating at startup. |
| A logging library (`zap`, `zerolog`) | `log/slog`. Structured, in the standard library, and this service will never log at a rate where the difference is measurable. |
| An HTTP router (`chi`, `gorilla/mux`) | `net/http.ServeMux`, which has had method and wildcard patterns since Go 1.22. |
| An OPML library | ~100 lines of `encoding/xml` in `internal/feed/opml.go`. The format is small and the available libraries disagree with real exports about as often as they agree. |
| An assertion library (`testify`) | `if got != want { t.Errorf(...) }`. No dependency, and the failure output says what was compared. |
| A test-container library | `internal/dbtest`, which skips when `TOME_TEST_DATABASE_URL` is unset. CI supplies a service container; nothing needs to orchestrate Docker from inside a test. |

## Expected dependencies

Named here in advance so that adding one is a decision that was already made
and reviewed, rather than a surprise in a diff. Each moves into the list above,
with its justification, in the milestone that introduces it.

| Dependency | Milestone | Why it will be needed |
|---|---|---|
| `go-shiori/go-readability` and/or `markusmobius/go-trafilatura` | M2 | Article extraction. This is the problem the project exists to solve well, and both encode years of accumulated heuristics. |
| `microcosm-cc/bluemonday` | M2 | HTML sanitization. Hand-rolled sanitizers are a documented source of XSS; this one is the Go ecosystem's reviewed answer. |
| An image codec for AVIF/WebP | M3 | The standard library encodes neither, and the asset policy depends on modern codecs to keep the archive's storage growth tolerable. |
| `chromedp` | M8 | Driving a headless browser for the small set of domains that render their content in JavaScript. |

## Build and CI dependencies

These are not linked into the binary and carry a different risk profile: a
failure breaks the build, never a running archive.

| Tool | Why |
|---|---|
| [Task](https://taskfile.dev) | The build runner. None of this is file-dependency work, so `make`'s model buys nothing; Task is a list of named commands, which is what this is. |
| [golangci-lint](https://golangci-lint.run) | Linter aggregator. The enabled set is in `.golangci.yml`, chosen to catch defects rather than style. |
| `gcr.io/distroless/static-debian12` | Runtime base image. No shell, no package manager, no libc — the attack surface is the binary and nothing else. |
