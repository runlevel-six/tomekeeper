# Dependencies

Every dependency needs a one-line justification here. This is not ceremony:
this is a single-maintainer project intended to hold a decade of reading, and
each dependency is something that can be abandoned, break compatibility, or
acquire a supply-chain problem while nobody is looking.

The bar is: *the standard library cannot do this, or doing it by hand would be
worse than the risk of the dependency.*

## Go module dependencies

**None.** As of M0 `go.mod` has no `require` block.

This is not an aspiration to keep forever — feed parsing and article extraction
are exactly the problems worth importing rather than writing. It is a statement
that nothing so far has needed one:

| What could have been imported | What is used instead |
|---|---|
| A CLI framework (`cobra`, `urfave/cli`) | A `switch` on `os.Args[1]`. There are three subcommands and no flags. A framework here would be more code to read, not less. |
| A config library (`viper`, `envconfig`) | ~100 lines in `internal/config`. Reflection-driven loaders make the error messages worse, and the error messages are the whole point of validating at startup. |
| A logging library (`zap`, `zerolog`) | `log/slog`. It is structured, it is in the standard library, and this service will never be logging at a rate where the performance difference is measurable. |
| An HTTP router (`chi`, `gorilla/mux`) | `net/http.ServeMux`, which has had method and wildcard patterns since Go 1.22. |
| An assertion library (`testify`) | `if got != want { t.Errorf(...) }`. Verbose, no dependency, and the failure output says what was compared. |

## Expected dependencies

Named here in advance so that adding one is a decision that was already made
and reviewed, rather than a surprise in a diff. Each moves into the list above,
with its justification, in the milestone that introduces it.

| Dependency | Milestone | Why it will be needed |
|---|---|---|
| `jackc/pgx` | M1 | The PostgreSQL driver. `database/sql` alone cannot use the Postgres-specific types and the binary protocol this schema relies on. |
| `riverqueue/river` | M1 | Postgres-backed job queue. Writing a correct one — visibility timeouts, retries, unique jobs — is a project in itself. |
| A migration tool (`golang-migrate` or `goose`) | M1 | Ordered, versioned, irreversible-by-default schema changes. The choice between the two is recorded in the data model reference. |
| `mmcdole/gofeed` | M1 | RSS, Atom, and JSON Feed parsing, including the malformed real-world feeds that make writing this yourself a permanent tax. |
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
