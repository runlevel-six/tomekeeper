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

## Adding real pages

The fixtures here are hand-built to exercise specific structures. They are not
a substitute for the real corpus the plan calls for — roughly thirty pages from
sites actually being read, which is what makes extraction quality measurable
against reality rather than against what the fixtures' author imagined.

To add one:

```sh
curl -sL 'https://example.com/the-article' -o internal/extract/testdata/pages/example-com.html
```

Then write `example-com.want` with the URL and a handful of distinctive
sentences from the article body, plus `!` lines for any navigation or footer
text that must not leak in. Run `go test ./internal/extract/` and the case is
live.

Keep the pages small where possible and prefer sites whose terms allow it.
These files are committed, so they are published with the repository.
