# Dependencies

Every dependency needs a one-line justification here. This is not ceremony:
this is a single-maintainer project intended to hold a decade of reading, and
each dependency is something that can be abandoned, break compatibility, or
acquire a supply-chain problem while nobody is looking.

The bar is: *the standard library cannot do this, or doing it by hand would be
worse than the risk of the dependency.*

## Go module dependencies

Seventeen libraries across nineteen modules — River publishes its driver and type
packages separately. Each is here because the standard library cannot do the job
and doing it by hand would be worse than the risk of the dependency.

The count is of *direct* dependencies. `go list -m -f '{{if not .Indirect}}…'`
is the authority; if it disagrees with this table, the table is wrong.

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
| `github.com/gen2brain/avif` | AVIF encoding, which is where most of the archive's storage saving comes from. Encodes through a WebAssembly build of libavif on the same wazero runtime trafilatura already pulls in, so no C toolchain is involved and the libaom bindings are avoided. **Must be built with `-tags nodynamic`:** it otherwise pulls in `ebitengine/purego` for an optional path that `dlopen`s a system libavif, and purego's `//go:cgo_import_dynamic` makes the binary link against libc *even with `CGO_ENABLED=0`* — which the distroless-static runtime image cannot exec. The tag is set in the Dockerfile, the Taskfile, and CI. |
| `github.com/HugoSmits86/nativewebp` | WebP encoding, as the fallback when AVIF encoding fails. Pure Go, for the same reason. It is lossless-only, which is why the pipeline discards any transcode larger than its source. |
| `golang.org/x/image` | WebP *decoding*, and the CatmullRom scaler used to downscale. The standard library has neither. Also what the brand assets are resized with — see [The mark and the lockup](../reference/logo.md). |
| `golang.org/x/net/html` | The HTML tokenizer the extraction ladder walks. Already in the tree beneath the extractors and `goquery`; the standard library has no HTML parser. |
| `github.com/prometheus/client_golang` | The metrics endpoint and its custom collector. Writing the exposition format by hand is a morning's work and then a decade of tracking a specification that changes without you; this is the reference implementation, and the collector interface is what lets the archive's gauges be queried on scrape rather than maintained continuously. |
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

## Vendored browser assets

Not Go modules, and carrying a different risk: these are bytes committed to the
tree and embedded in the binary, so they cannot break at build time or drift
underneath a running archive. What they can do is bloat every page load, which is
why the list is short and why each file is subset.

None of them is loaded from a CDN. A page that references someone else's host
fails offline, leaks the reader to a third party, and bets the archive on a company
still serving that exact path in ten years — the same bet the
files-are-the-archive principle exists to avoid. `internal/server/static/vendor/README.md`
holds the versions, checksums, and upgrade recipes.

| Asset | Why |
|---|---|
| htmx 2.0.9 | Swapping one control's markup after a POST, without writing a fetch-and-replace layer by hand. Every form it enhances also works submitted plainly, so this is progressive rather than load-bearing. |
| Literata (variable, Latin subsets) | Prose. The alternative is a `ui-serif` stack, which makes the reading experience whatever serif the reader's OS nominates — and on Windows and much of Linux that is markedly worse than Georgia. A tool whose entire purpose is reading cannot leave its typeface to chance. OFL-1.1. |
| Inter (variable, Latin subsets) | The interface. Built out of 11–14px letter-spaced uppercase labels, which is where a UI sans earns its keep and where a book face goes soft. OFL-1.1. |

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
