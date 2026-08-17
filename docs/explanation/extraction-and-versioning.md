# Extraction and versioning

Turning a fetched web page into a readable article is a heuristic, and every
heuristic is wrong sometimes. The design here assumes that from the start: it
treats an extracted body as a *view* that can be regenerated, never as the
thing being kept.

## The raw fetch is the artifact

When a page is fetched, the original bytes are gzipped and written to the blob
store, and the article row records where. The extracted body is derived from
that copy, not from the network.

This costs disk — roughly a fifth of the page after compression, kept forever,
for every article — and it is the single most important thing M2 does.

The reason is arithmetic. Extraction improves: a library gets better, a
threshold gets tuned, someone writes a rule for a site that never worked. If
re-extraction required re-fetching, then every improvement would apply only to
articles fetched *after* it. Ten years in, the improvements would have reached
the newest 5% of the archive and none of the rest — and the older articles,
whose sites have since been redesigned, paywalled, or deleted, are exactly the
ones that cannot be fetched again at any price.

Keeping the bytes means `tome reextract` can rebuild the entire archive at the
current extractor version, offline, without asking a single server for
anything. That is what makes the archive able to get better with age instead of
merely older.

## The ladder

Extraction runs in order and stops at the first acceptable result:

1. **A domain rule's CSS selector**, if the host has one.
2. **go-trafilatura**, the primary extractor.
3. **go-readability**, the fallback.
4. *Headless rendering* — M8, for domains flagged `requires_js`.
5. **The feed's own body**, if everything else failed.

Two extractors rather than one because they fail differently. Trafilatura has
better precision on news and blog markup; readability handles some older,
table-heavy layouts that trafilatura discards. Running both costs
milliseconds against a page already in memory.

Trafilatura's own internal fallback to a readability-style pass is turned
**off**, even though it exists. If it were on, a readability result would be
recorded as `extractor_name = "trafilatura"`, and the per-extractor statistics
that show which extractor is actually earning its place would be measuring
nothing.

### What counts as acceptable

A result is accepted when the extracted text is **at least 200 characters** and
**at least 25% of the page's visible text**.

Both halves are load-bearing, because there are two ways to fail:

- Too little. A paywall stub, a cookie wall, or an empty JavaScript shell
  produces a short body that is technically valid and worthless.
- Too much. An extractor that gives up and returns the whole page — navigation,
  sidebar, footer, "related stories" — looks excellent by length alone. The
  ratio catches it: an article is a large share of its page, but the *page* is
  not a large share of itself once chrome is included.

The denominator excludes `<script>`, `<style>`, `<noscript>`, and `<template>`
contents, which are text nodes in the parse tree but are not visible. Counting
them would inflate the denominator and reject good extractions from
script-heavy sites.

A domain rule overrides the ratio check. A rule is a human saying "the body is
here", and the entire reason one exists is that the heuristics were wrong about
this site.

### The feed body rung

The last rung is not compared against the page at all, because it is a
different document — comparing them would reject every feed body for being
short relative to a page it does not come from. It still has to clear the
200-character floor: a two-sentence teaser is not an article, and storing it as
one would make the archive look complete when it is not.

This rung matters more than its position suggests. When a site goes down
between publication and the next poll, the feed's copy is the only copy — and
that is precisely the moment archiving is worth having.

## Versioning

Every body records the extractor that produced it and the version of the
extraction *behavior*, a constant in the code:

```go
const Version = "1"
```

**Bump it whenever extraction output could change** — a new rung, a changed
threshold, a different sanitization policy, an upgraded extractor library.
`tome reextract --since-version <n>` finds everything produced by older
behavior and queues it. Forgetting to bump it means an improvement silently
never reaches the archive it was written for.

A new body does not overwrite the old one; it **demotes** it. `is_current`
moves to the new row and the previous one stays. That costs a row and buys the
ability to diagnose a bad extractor release after the fact, by looking at what
the same page produced before.

### Immutable bodies

An imported body — from Wallabag, from any future adapter — is flagged
`immutable`, and the rules around it are absolute:

- A bulk reprocess never selects it. This is enforced by `NOT c.immutable` in
  the candidate query, not by a check in the loop that walks the results. A
  `WHERE` clause is a proof; a conditional in a loop is a promise.
- A later successful fetch of the same URL is stored **beside** it, not over
  it, and does not become current. Promoting one over the other is a deliberate
  human act.

The reasoning is that an imported body may be the only surviving copy of a page
that no longer exists. Re-fetching a dead URL produces a parking page, and an
automatic "improvement" that replaced a ten-year-old saved article with a
domain-squatter's landing page would be a silent, unrecoverable loss.

## Sanitization

Extracted HTML is sanitized with an allowlist policy before it is stored:
bluemonday's UGC policy, plus the figure and responsive-image elements that
articles actually use, plus the semantic elements that carry structure.

The threat model is not hypothetical. The archive renders markup that arbitrary
websites authored, in the reader's browser, on the reader's own origin, and it
will keep doing so for a decade. A `<script>` that survives extraction runs with
whatever session the reader has. So:

- Only `http` and `https` URLs survive, which is what rejects `javascript:`,
  `data:`, and `file:` references.
- Links get `rel="nofollow noreferrer"` and open in a new tab, so the archive
  does not leak the reader's location to sites it links to.
- Every reference is resolved to an absolute URL *before* sanitization, against
  the article's own address. A relative link in stored markup would otherwise
  resolve against whatever page displays it — and M3's asset pipeline works
  from the stored body, so it needs absolute image URLs to fetch.

## Where the tail lives

Readability-class tools handle most sites. The rest need hand-written rules,
permanently — this is a known, accepted, unfixable property of the problem, not
a bug that better code will eventually close.

`domain_rules` and the failed-fetch queue exist to make that routine: look at
what failed, find the selector, write one rule, re-extract. See [How to add a
domain rule](../how-to/add-a-domain-rule.md).

## See also

- [Reprocess the archive](../how-to/reprocess-the-archive.md)
- [Politeness and rate limiting](politeness-and-rate-limiting.md)
- [Data model](../reference/data-model.md#article_content)
