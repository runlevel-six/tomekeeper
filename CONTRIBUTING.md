# Contributing to tomekeeper

Thanks for your interest. tomekeeper is a feed reader that permanently archives
every article it ingests, and the most valuable contributions right now are
**pages it cannot read** — the web is far more varied than any one person's
subscription list, and every site that defeats the extractors is a site nobody
here has seen.

## Ways to help that don't involve writing Go

- **Report a page that produced no body.** The URL plus the output of
  `tome explain <article-id>` is usually enough: it reports what every rung of
  the extraction ladder produced and which threshold turned it down.
- **Say which kind of failure it is**, because the two have opposite fixes and
  the archive now measures the difference. A page that served a few hundred
  characters of visible text is a JavaScript shell and wants a browser; one that
  served thousands is a page whose structure defeated the extractors and wants a
  CSS selector. The failed-fetch queue shows that number against every row.
- **Contribute a domain rule.** See
  [Add a domain rule](docs/how-to/add-a-domain-rule.md). One caveat worth
  knowing before you write one: a rule supersedes *every* rung for its whole
  host, including rungs that do something no rule can — so on a host that mostly
  works already, check what the new bodies lose as well as what they gain.
- **Report an importer failure.** Please don't attach your export: a Wallabag or
  FreshRSS file is a subscription list and a complete reading history. One
  redacted record that reproduces the problem is worth more and costs you
  nothing.
- **Tell us your deployment shape** — how you run Postgres, what storage class,
  whether you use the Docker Compose or Kubernetes path. The manifests are only
  as good as the range of deployments we know about.

## Development

```sh
git clone https://github.com/runlevel-six/tomekeeper.git
cd tomekeeper
task hooks:install   # adds the DCO sign-off trailer for you; see below
task check           # everything CI runs: fmt, vet, lint, pins, sign-off, js, test, build
task build           # bin/tome
```

Requires Go (see `go.mod` for the minimum) and [Task](https://taskfile.dev).
`task --list` shows every target.

`task test:js` runs the touch gestures in `static/tome.js` against a stub DOM. It is
node and nothing else — no package.json, no framework, no build step — and it exists
because those gestures have guards nobody can check by hand.

**Two things about the build and the test suite will mislead you if nobody says
them out loud:**

1. **A green `go test ./...` may only be the unit half.** Every test that needs
   PostgreSQL skips itself when `TOME_TEST_DATABASE_URL` is unset, which keeps
   the suite runnable anywhere at the cost of a pass that means less than it
   looks like. Run `task test:integration` to make a missing database an error
   instead of a skip. CI runs a `postgres:16-alpine` service and then fails the
   job if the integration tests skipped anyway, because a suite that silently
   stops covering things is worse than one that never covered them.
2. **The binary must be built `CGO_ENABLED=0 -tags nodynamic`.** Without the
   tag, the AVIF encoder pulls in a library that reaches `dlopen` through
   `//go:cgo_import_dynamic`, so the binary needs `libc.so.6` even with cgo
   disabled — and the runtime image has no libc. The symptom is
   `exec /usr/local/bin/tome: no such file or directory` on a file that plainly
   exists, which is the ELF interpreter missing rather than the binary. The
   Taskfile, the Dockerfile and CI all set it. **Do not** set `CGO_ENABLED=0` on
   the test tasks: `go test -race` needs cgo.

## Architecture in one paragraph

Feeds are polled on an adaptive interval. Each new item becomes an article, and
then three jobs run in sequence on [River](https://riverqueue.com) inside the
same PostgreSQL database: `fetch_article` stores the page's bytes verbatim in a
content-addressed blob store, `extract_article` runs a ladder of extractors over
those stored bytes, and `localize_assets` downloads the images the resulting body
references so the archive does not depend on anybody else's server. Articles are
the root entity rather than feed items, so a story syndicated by three feeds is
one archived article — see
[Why articles are the root entity](docs/explanation/why-articles-are-the-root-entity.md).

**The rule that matters:** the stored fetch is the archive, and a body is a
derived, versioned view of it. Nothing in `internal/extract` touches the network,
so extraction can be re-run over the whole archive at any time — which is the
only reason an improvement written today can reach an article saved two years
ago. The corollary is a hard requirement: **anything that changes what extraction
produces must bump `extract.Version`.** That constant is what `tome reextract`
selects on, so forgetting it means the improvement silently never reaches the
articles it was written for. See
[Extraction and versioning](docs/explanation/extraction-and-versioning.md).

**The other rule that matters:** every query that returns articles is scoped to
one reader by a single shared SQL predicate, and cross-reader queries live only
on `Store.System()`. Do not inline a second copy of that predicate and do not
widen it. A "forbidden" response for another reader's article is also a bug — it
confirms the article exists. See
[Scoping and access control](docs/explanation/scoping-and-access-control.md).

## Changing extraction

The ladder tries a domain rule, then trafilatura, then go-readability, then the
feed's own body, then the page's images, and takes the first acceptable result.
If you are adding a rung or moving a threshold:

- **Bump `extract.Version`** and say so in the changelog under its own heading.
  It is the one kind of change that wants a follow-up command (`tome reextract`)
  from everyone who upgrades.
- **There are two corpora.** Synthetic fixtures under
  `internal/extract/testdata/pages/` are committed and are what everyone runs.
  Real captured pages are third-party content and live outside the tree at
  `TOME_TEST_CORPUS_DIR`; `task test:corpus` runs them and fails if that is
  unset. `tome corpus add <url>` captures a page into it.
- **Neuter your own guard.** Every new threshold or scoping clause needs a test
  that fails when you delete *just that clause* — several tests in this
  repository have turned out to be passing for the wrong reason, always because
  a broader condition was carrying them. If deleting the line under test leaves
  the suite green, the test is describing the neighbours.
- **Measure before and after on real bodies, on something other than length.** A
  change that improves the word count can still throw away the part that
  mattered.

## Adding a migration

Migrations are append-only and numbered, in `internal/db/migrations/`. Adding one
forces a minor release rather than a patch, because
[CHANGELOG.md](CHANGELOG.md) promises that a patch upgrade is only a tag change;
`scripts/check-release.sh` enforces that rather than trusting anybody to remember
it. Prefer a nullable column over a default when NULL and the default would mean
different things — "not measured yet" is not the same claim as zero.

## Commit messages

`type(scope): subject`, in the imperative, with a prose body explaining why. Read
`git log` for the register. The changelog is written by hand rather than
generated from commit subjects, so the subject line is for the next person
reading `git log`, not for tooling — which makes it worth more, not less.

Releases are annotated tags; see [Cut a release](docs/how-to/cut-a-release.md).

## Developer Certificate of Origin

Contributions require a DCO sign-off. Add a `Signed-off-by` line with
`git commit -s`:

```text
Signed-off-by: Your Name <your.email@example.com>
```

This certifies that you wrote the patch or have the right to submit it under
the project's license. Full text: <https://developercertificate.org/>

`task hooks:install` makes this automatic — it points `core.hooksPath` at
`.githooks/`, whose `prepare-commit-msg` adds the trailer when it is missing.
Passing `-s` as well is harmless; an identical trailer is never duplicated. Merge
commits are left alone, because someone else's commits are not yours to certify.

CI checks it too, over the commits a push or a pull request added — a hook only
helps whoever installed it. `task dco` runs the same check locally, and it will
tell you how to add a trailer you forgot.

## License

By contributing, you agree your contributions are licensed under the GNU Affero
General Public License v3.0. See [LICENSE](LICENSE).
