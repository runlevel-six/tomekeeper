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
for every article — and it is the single most important thing the fetcher does.

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
4. *Headless rendering* — planned, for domains flagged `requires_js`.
5. **The feed's own body**, if everything else failed.
6. **The page's own images**, for articles that are a picture rather than prose.

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

### The ratio check stops applying to long bodies

Past 2,000 characters the ratio is not consulted at all. This is a correction
made against real data rather than an original design choice, and it is worth
recording why.

A documentation site carries its entire table of contents on every page. On one
measured against the real feed list, the sidebar alone came to roughly 40,000
characters of visible text, so whole-page visible length sat between 45,000 and
54,000 for every article while the posts themselves ran 3,500 to 13,300
characters. Every single one scored below 0.25 and was rejected. **42 of 50
articles fell through to the feed body**, with the full page sitting on disk the
whole time.

The ratio was not measuring the quality of the extraction. It was measuring the
length of the post against the length of the sidebar — a property of the site's
template, which changes whenever the site adds a documentation section.

What settles the threshold is that the two failure modes are not symmetric:

- A **false reject** stores a truncated feed summary and discards a good body.
  That is the failure the whole project exists to prevent, and it is silent —
  nothing logs an error and the article looks present.
- A **false accept** stores a short but real body. Because the raw page is kept
  (the store-the-raw-fetch principle above), it can be extracted again later with better heuristics.

One is recoverable and one is not, so the absolute length wins where it can.
Below 2,000 characters the question is genuinely open — a short run of text on a
sparse page really can be a cookie notice — and there the ratio still decides.
`testdata/pages/chrome-heavy.html` locks the behavior in: with the length
exemption removed, that fixture fails with `no extractor produced acceptable
content`, which is exactly what the real pages did.

### The feed body rung

The last rung is not compared against the page at all, because it is a
different document — comparing them would reject every feed body for being
short relative to a page it does not come from. It still has to clear the
200-character floor: a two-sentence teaser is not an article, and storing it as
one would make the archive look complete when it is not.

This rung matters more than its position suggests. When a site goes down
between publication and the next poll, the feed's copy is the only copy — and
that is precisely the moment archiving is worth having.

### The page images rung

Webcomics defeat every rung above, and not by a small margin. Each of those
extractors scores by text density, and a comic has almost none — so they do not
rank the strip poorly, they cannot see it at all, and settle on the largest block
of words on the page instead. On one strip that was the news sidebar: seventy-
eight words of shop announcements, stored as the article, with the comic absent
entirely.

The acceptance floor then closed the last door. `minChars` rejects anything under
200 characters as a paywall stub or a navigation shell, which is right for text
and exactly wrong here, so even a hand-written domain rule pointing straight at
the strip produced nothing. This rung does not consult the floor.

Telling the comic from the furniture is the real problem, on pages carrying
twenty images of navigation arrows, logos and banners. The signal is that a
content image's URL contains the article's own slug, while chrome lives under
generic paths shared by every page on the site:

| Article | Its image | Chrome on the same page |
|---|---|---|
| `/comics/oots1347.html` | `/comics/strip/**oots1347**_….png` | `/redesign/ComicNav_Next.gif` |
| `/2016/**project-lifecycle**` | `/2016/project-lifecycle/4-….png` | `/images/logo.png` |
| `/comics/**design_hell**` | `/comics/design_hell/1.png, 2.jpg` | `/default/header_2023/….png` |

It is a precise signal rather than a clever one, and that is the point: a false
positive stores a banner in place of an article, so the rung would rather find
nothing and fall through than guess. Sites whose image URLs share nothing with
their page URLs are not rescued and still need a domain rule.

**Last, and after the feed body, on purpose.** A page whose text extraction
failed is usually paywalled or JavaScript-rendered rather than a comic, and for
those the feed's words are worth more than the article's hero image. Ordering
this rung earlier would trade real prose for a picture on every one of them.

### Second-guessing a rung that succeeded

Two corrections apply to a rung that already declared success, because
"acceptable" is a floor rather than a judgement — a page's header block or its
navigation sidebar clears a floor as easily as an article does.

**A much richer feed body wins.** Observed on a real article where trafilatura
returned 30 words — the title twice and two dates — while the feed carried the
whole 2,000-word piece, which was then discarded. When the feed body is three
times richer than the page extraction, the feed wins. This is safe in one
direction only, which is what makes it sound: a feed summary is a *truncation* of
the article, so it cannot legitimately be several times longer than the article's
own body. It can only ever move toward the longer text, never the shorter, so it
cannot cause the truncated-summary failure this whole ladder exists to prevent.

**A thin, imageless body loses to the page's images.** Three conditions, all
required, because replacing a body is destructive: the body carries no image at
all, it is under 120 words, and the page has images bearing the article's slug. A
body that already contains an image is left alone whatever its length —
extraction found the picture, and there is nothing to add.

## Versioning

Every body records the extractor that produced it and the version of the
extraction *behavior*, a constant in the code:

```go
const Version = "3"
```

**Bump it whenever extraction output could change** — a new rung, a changed
threshold, a different sanitization policy, an upgraded extractor library.
`tome reextract --since-version <n>` finds everything produced by older
behavior and queues it. Forgetting to bump it means an improvement silently
never reaches the archive it was written for.

| Version | Change |
|---|---|
| `1` | The ladder as originally specified. |
| `2` | The ratio check no longer applies to bodies past 2,000 characters (2026-08-17). |
| `3` | A much richer feed body wins over a thin page extraction; a page images rung for webcomics (2026-08-18). |

Note what the version does **not** cover: adding or editing a *domain rule*
changes extraction output without changing this constant, so `reextract` will not
select the affected articles on its own. Use `--since-version 0`, which matches
every body because no body carries version `0`, together with `--domain` to keep
the work to the site the rule affects:

```sh
tome reextract --since-version 0 --domain example.com
```

That asymmetry is worth understanding rather than working around. The version
constant tracks *this program's* extraction behavior, and a domain rule is data
rather than behavior — it can change between two runs of the same binary. Making
rule edits bump a compiled-in constant is impossible; making the version a
database value would mean every rule edit invalidated the whole archive's bodies.
Two flags is the honest interface.

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
  resolve against whatever page displays it — and the asset pipeline works
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
