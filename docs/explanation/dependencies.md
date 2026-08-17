# Dependencies

Every dependency needs a one-line justification here. This is not ceremony:
this is a single-maintainer project intended to hold a decade of reading, and
each dependency is something that can be abandoned, break compatibility, or
acquire a supply-chain problem while nobody is looking.

The bar is: *the standard library cannot do this, or doing it by hand would be
worse than the risk of the dependency.*

## Go module dependencies

Twelve libraries across fourteen modules — River publishes its driver and
type packages separately. Each is here because the standard library cannot do
the job and doing it by hand would be worse than the risk of the dependency.

| Dependency | Why |
|---|---|
| `github.com/jackc/pgx/v5` | The PostgreSQL driver. `database/sql` cannot use the binary protocol or the Postgres-specific types this schema relies on, and pgx is the maintained choice in Go. Its `stdlib` subpackage also gives goose the `*sql.DB` it wants without a second connection pool. |
| `github.com/pressly/goose/v3` | Schema migrations. Chosen over `golang-migrate` because it works as a *library* against an embedded `fs.FS`: the runtime image is distroless, with no shell and no filesystem to read `.sql` files from, so migrations have to travel inside the binary that expects them. |
| `github.com/riverqueue/river` | Postgres-backed job queue, with `riverdriver/riverpgxv5` and `rivertype`. Writing a correct queue — visibility timeouts, retries with backoff, unique jobs, periodic schedules — is a project in itself, and this one keeps the promise of a single stateful dependency. |
| `github.com/mmcdole/gofeed` | RSS, Atom, and JSON Feed parsing. The specifications are only half the problem; the other half is the decade of malformed real-world feeds this library already handles, which is a tax paid forever if written from scratch. |
| `github.com/markusmobius/go-trafilatura` | The primary article extractor. This is the problem the project exists to solve well, and it encodes years of accumulated heuristics that cannot be reproduced by reading a specification. It is also the heaviest dependency here — it pulls a WebAssembly runtime for its language detection — which is the price of the accuracy. |
| `github.com/go-shiori/go-readability` | The fallback extractor. It fails differently from trafilatura, particularly on older table-heavy layouts, and running both against a page already in memory costs milliseconds. |
| `github.com/microcosm-cc/bluemonday` | HTML sanitization. Hand-rolled sanitizers are a documented source of cross-site scripting, and this archive renders markup written by arbitrary websites in the reader's own browser for a decade. This is the Go ecosystem's reviewed answer. |
| `github.com/PuerkitoBio/goquery` | CSS selector matching, for domain rules and for URL resolution in extracted bodies. Already in the tree as a transitive dependency of the extractors, so using it directly adds nothing. |
| `github.com/temoto/robotstxt` | robots.txt parsing. The format has more edge cases than it appears to — wildcard paths, longest-match precedence, agent matching — and getting them wrong means either ignoring a site's wishes or refusing pages it never restricted. |
| `golang.org/x/time/rate` | The per-host token bucket. A correct rate limiter with a burst allowance is not hard, but this one is already in the extended standard library and already correct. |
| `github.com/gen2brain/avif` | AVIF encoding, which is where most of the archive's storage saving comes from. Pure Go via a WebAssembly runtime, so it works under `CGO_ENABLED=0` and the image stays distroless-static — the libaom bindings would have cost that. It reuses the same wazero runtime trafilatura already pulls in. |
| `github.com/HugoSmits86/nativewebp` | WebP encoding, as the fallback when AVIF encoding fails. Pure Go, for the same reason. It is lossless-only, which is why the pipeline discards any transcode larger than its source. |
| `golang.org/x/image` | WebP *decoding*, and the CatmullRom scaler used to downscale. The standard library has neither. |
| `golang.org/x/crypto/argon2` | Password hashing (argon2id). The standard library has no memory-hard KDF, and this is the reference implementation for the algorithm current guidance recommends. Only the KDF is taken from it: the PHC string encoding, parameter parsing, and constant-time comparison are written here, so the encoding is inspectable and no dependency owns the on-disk format of a credential. |
| `golang.org/x/sync/singleflight` | Collapsing concurrent fetches of the same image URL across articles. The database lookup that dedupes fetches is a check-then-act, so without this the origin serves one request per worker slot for a picture shared between articles. Already in the tree as a transitive dependency, and a hand-rolled map of in-flight keys is the kind of thing that looks right and leaks a goroutine on the error path. |

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
with its justification, when the work that needs it lands.

| Dependency | For | Why it will be needed |
|---|---|---|
| `chromedp` | headless rendering | Driving a headless browser for the small set of domains that render their content in JavaScript. |

## Build and CI dependencies

These are not linked into the binary and carry a different risk profile: a
failure breaks the build, never a running archive.

| Tool | Why |
|---|---|
| [Task](https://taskfile.dev) | The build runner. None of this is file-dependency work, so `make`'s model buys nothing; Task is a list of named commands, which is what this is. |
| [golangci-lint](https://golangci-lint.run) | Linter aggregator. The enabled set is in `.golangci.yml`, chosen to catch defects rather than style. |
| `gcr.io/distroless/static-debian12` | Runtime base image. No shell, no package manager, no libc — the attack surface is the binary and nothing else. |
