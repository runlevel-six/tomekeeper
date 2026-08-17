# Extraction corpus

Each case is two files with the same stem:

- `<name>.html` — a saved page, exactly as fetched
- `<name>.want` — a text file whose first lines are `key: value` headers,
  followed by a blank line and then substrings the extracted text **must**
  contain, one per line. A line beginning with `!` is a substring the extracted
  text **must not** contain.

Recognized headers:

| Header | Meaning |
|---|---|
| `url` | The article URL, used to resolve relative references. Required. |
| `extractor` | Which rung must win: `domain_rule`, `trafilatura`, `readability`, `feed_body`. Optional; unset means any is acceptable. |
| `min_chars` | The extracted text must be at least this long. Optional. |
| `selector` | A domain-rule content selector to apply. Optional. |
| `strip` | A domain-rule strip selector. May be repeated. Optional. |
| `feed_body` | Path to a file holding the feed's own body, for the fallback rung. Optional. |
| `expect` | `content` (default) or `none` — the latter asserts that extraction fails. |

The `!` lines are the ones that catch regressions worth catching. An extractor
that starts including navigation, cookie banners, or "related stories" still
looks fine by length; it only shows up as forbidden text appearing in the body.

## Two corpora

The fixtures in this directory are hand-built to exercise specific structures —
one rung of the ladder each, plus one hostile page for the sanitizer. They are
committed and published, and they are what a contributor runs.

They are not a substitute for the real corpus the plan calls for: roughly thirty
pages from sites actually being read, which is what makes extraction quality
measurable against reality rather than against what the fixtures' author
imagined. **Those pages are not in this repository.** They are third-party
copyrighted content, so they live in a private directory outside the tree and
are loaded on demand.

| | Committed fixtures | Private corpus |
|---|---|---|
| Location | this directory | `$TOME_TEST_CORPUS_DIR` |
| Purpose | one structure each, exhaustive on the ladder | breadth, real-world messiness |
| Who runs it | everyone | whoever has the directory |
| Published | yes | no |

Both use the file format above, so a case can be moved between them by moving
two files. Stems must be unique across the two: a collision is a hard failure
rather than one case silently shadowing the other.

## Adding a page to the private corpus

Keep the corpus in its own directory — a private Git repository is the usual
choice, since it wants the same version history as the extractor it guards. Put
it **outside** this tree; a gitignored directory inside the tree does not survive
a fresh clone, which is exactly when you would want the corpus.

```sh
export TOME_TEST_CORPUS_DIR=~/src/tomekeeper-corpus
curl -sL 'https://example.com/the-article' -o "$TOME_TEST_CORPUS_DIR/example-com.html"
```

Then write `example-com.want` with the URL and a handful of distinctive sentences
from the article body, plus `!` lines for any navigation or footer text that must
not leak in.

```sh
task test:corpus
```

That target fails if the variable is unset, rather than quietly running six
fixtures and reporting success. `go test ./...` still skips the private corpus
when the variable is absent, the same way the Postgres integration tests skip —
see `internal/dbtest` for the same pattern and the same reasoning.

`TestCorpus` logs its case counts on every run:

```
corpus: 6 committed fixtures, 31 private pages
```

Watch that second number. A corpus that stops being found looks identical to a
passing suite.

## Adding a page to the committed fixtures

Do this when the page demonstrates a *structural* case the existing six do not
cover, and when its content can be published — hand-written markup, or a page
whose terms clearly allow redistribution. Keep it small. Anything saved from a
site you simply happen to read belongs in the private corpus instead.
