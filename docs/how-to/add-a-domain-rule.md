# How to add a domain rule

Some sites extract badly. The heuristics handle most of the web, and the rest
need a hand-written rule — permanently. This is routine maintenance, not a bug
you are working around.

## Find the articles that need one

```sql
SELECT substring(url_canonical from 'https?://([^/]+)') AS domain,
       count(*) AS failures,
       max(fetch_error) AS example_error
FROM articles
WHERE fetch_status IN ('failed', 'skipped')
GROUP BY 1
ORDER BY failures DESC
LIMIT 20;
```

Domains at the top of that list are where a rule buys the most.

A different failure mode is extraction that "succeeds" but returns the wrong
thing. Those articles have a body, just a short or strange one:

```sql
SELECT a.url_canonical, c.extractor_name, length(c.content_text) AS chars
FROM articles a
JOIN article_content c ON c.article_id = a.id AND c.is_current
WHERE length(c.content_text) < 500
ORDER BY chars
LIMIT 20;
```

## Ask what the ladder already did

Before guessing at a selector, ask. `tome explain` runs the ladder over the page
already stored — no requests — and reports what every rung produced and which
threshold turned it down:

```console
$ tome explain 1267
article 1267: https://example.com/2026/08/a-post
  stored page: pages/ab/cd/abcdef.html.gz (129 KB uncompressed)
  fetch: failed — extraction produced no content

  RUNG         CHARS  WORDS  IMAGES  OUTCOME
  page         41904  0      0       measured: 41904 characters of visible text; a body under 2000 characters must be at least 25% of it (10476 characters)
  domain_rule  0      0      0       skipped: no rule for this domain
  trafilatura  0      0      0       rejected: produced nothing
  readability  0      0      0       rejected: produced nothing
  feed_body    0      0      0       skipped: the feed carried no body for this article
  page_images  0      0      0       rejected: no image on the page is named after this article's slug or its title, so none of them is its content
```

The answer usually tells you which problem you have:

| What it says | What it means |
|---|---|
| Every rung `produced nothing`, and `page` is tiny | The stored page has no article in it — a JavaScript shell, or a fetch that landed on a consent wall. No selector will help. |
| Every rung `produced nothing`, and `page` is large | The text is there and the heuristics cannot find it. This is what a rule is for. |
| A rung was accepted but the body is short | Extraction "succeeded" on the wrong element. A rule replaces it. |
| `domain_rule` ran and was rejected | Your selector matched nothing, or matched too little. The line quotes the selector as saved, so it can be pasted straight back. |

Once a rule is written, run it again: the `domain_rule` row shows what the
selector actually picks up, and `--body` prints the opening of it.

To read the stored page itself:

```sh
zcat /var/lib/tomekeeper/articles/2026/08/the-slug-a1b2c3/raw.html.gz | less
```

The path is in `articles.raw_blob_path`, and `tome explain` prints it too.

## Work out the selector

Open the article in a browser, find the element that wraps the body, and note a
selector that identifies it. In the developer console:

```js
document.querySelector('div[data-role="story-body"]').innerText.length
```

A good selector is specific enough to exclude navigation and promotional
blocks, and general enough to still match next month. Prefer a semantic
attribute (`[data-role="story-body"]`, `article.post-content`) over a generated
class name (`.css-1a2b3c`), which changes on the site's next deploy.

### A body split across several blocks

Some sites break the article around mid-article advertising, emitting several
sibling blocks that each hold part of it:

```html
<div class="post-content"> …first third… </div>
<div class="ad-wrapper">…</div>
<div class="post-content"> …second third… </div>
<div class="ad-wrapper">…</div>
<div class="post-content"> …last third… </div>
```

The heuristics take one block and stop, which is why an article can look as though
it simply ended early — and why the same site can extract correctly on one article
and badly on the next, depending on where the first block happened to end. A rule
handles it: **every** element the selector matches is used, in document order, so
naming the class they share reassembles the article.

The lead image is often *outside* those blocks. A comma-separated selector picks
both up, and an element that falls inside another match is skipped rather than
emitted twice, so overlapping selectors do not duplicate a picture:

```sh
tome domain-rule set \
  --selector 'div.article-header > div.lightbox, .post-content' \
  --strip '.ad-wrapper' --strip '.related-stories' \
  example.com
```

Check the article's own length before and after. If the text roughly triples, the
page was one of these.

### The same picture twice

If an image appears twice in the stored article, the usual cause is not your
selector: it is the site shipping several sizes of one picture and hiding the extras
with CSS.

```html
<a href="/photo-full.jpg">
  <img src="/photo-640x427.jpg"  class="… hidden">    <!-- hidden on the site -->
  <img src="/photo-1152x648.jpg" class="intro-image"> <!-- the one you see -->
</a>
```

This archive stores the markup and renders it in its own styles, deliberately — so a
class that only meant something alongside the site's stylesheet means nothing here,
and both copies appear. Strip the hidden one:

```sh
tome domain-rule set --selector '…' --strip 'img.hidden' example.com
```

`img.hidden` rather than `.hidden`, deliberately: a site that hides a *text* block at
some screen width is hiding it from that layout, not from the article, and stripping
all hidden elements would delete content that a "read more" control was going to
reveal.

## Write the rule

### In the browser

**Rules** in the navigation, or the *Rule for …* link on the row in **Attention**
that sent you here. Fill in the body selector, put strip selectors one per line,
save, then press **Reprocess** on the rule's row — that second step is what applies
it to articles already stored, and the page says how many were queued.

The rest of this page uses the command line, which does the same thing and is what
a script or a runbook wants.

### At the command line

```sh
tome domain-rule set \
  --selector 'div[data-role="story-body"]' \
  --strip '.promo' \
  --strip '.newsletter-signup' \
  --notes 'body is in a data-role attribute; promos are inline in the flow' \
  example.com
```

**Either order works.** `set example.com --selector ...` and
`set --selector ... example.com` both save the same rule. Flags used to have to come
first, because argument parsing stopped at the first non-flag word and the natural
order printed the usage text instead of saving anything — that is fixed rather than
documented now.

| Flag | What it does |
|---|---|
| `--selector` | CSS selector for the article body. Extraction uses this instead of the heuristics. |
| `--strip` | Selector removed before extraction. Repeat for several. |
| `--rate` | Per-host requests per second, overriding `TOME_FETCH_RPS`. Use it for a site that has asked you to slow down. |
| `--requires-js` | Marks the domain as needing a headless render. Has no effect until headless rendering exists. |
| `--user-agent` | What this archive calls itself when it asks this site for a page. See [When a site refuses the default identity](#when-a-site-refuses-the-default-identity). |
| `--notes` | Why this rule exists. Write something; you will not remember. |

Rules apply to subdomains. A rule for `example.com` covers `blog.example.com`
and `www.example.com` unless that subdomain has a rule of its own, in which case
the more specific one wins. Check what would apply:

```sh
tome domain-rule show blog.example.com
```

```
blog.example.com (matched by the rule for example.com)
  selector: div[data-role="story-body"]
  strip:    .promo, .newsletter-signup
  requires js: no
  rate:     -
  user agent: -
  notes:    body is in a data-role attribute; promos are inline in the flow
```

## When a site refuses the default identity

Some origins filter on the *shape* of the User-Agent rather than on what a fetcher
does. The symptom is a whole host going from fetching cleanly to `HTTP 403` at once,
with nothing in their `robots.txt` to explain it and the feed still polling fine —
because the filter sits in front of the pages, not the feed.

Check before assuming. If a browser string is served and an honest one is not, it is
this:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -A 'tomekeeper/1.0.0' https://example.com/an-article/
curl -s -o /dev/null -w '%{http_code}\n' -A 'Mozilla/5.0 (compatible; Example/1.0; +https://example.com)' https://example.com/an-article/
```

If the first is 403 and the second is 200, the filter is rejecting honesty rather
than rejecting robots, and a per-domain identity gets past it without pretending to
be a person:

```sh
tome domain-rule set \
  --user-agent 'Mozilla/5.0 (compatible; tomekeeper; +https://your.contact)' \
  --notes 'refuses anything without a browser-shaped user agent' \
  example.com
```

That is the long-standing convention Googlebot and bingbot use: the `Mozilla/5.0`
token is vestigial and the parenthesis still says who is asking and how to reach
them. **Do not put a real browser's string here.** A rate limit and a contact URL
are what make an archiver welcome on somebody else's server, and a fetcher that
claims to be a person has given up both.

Leave the version out. The field is a fixed string, so one that names a version
starts lying about it at the next upgrade — and the default identity, which every
other host still gets, carries the version already.

Two things this needs that a selector does not:

- **A worker restart.** Per-domain identities are read once at startup, the same as
  rate limits.
- **A re-fetch, not a reprocess.** A recorded fetch failure is never retried, so the
  articles that already failed need asking for by hand — see
  [Articles that never fetched](#articles-that-never-fetched). Reprocessing does
  nothing for them; there is no stored page to reprocess.

## Apply it to what is already stored

A rule changes nothing on its own. Articles already extracted keep the body
they have until they are reprocessed:

```sh
tome reextract --target-version 0 --domain example.com
```

Both flags earn their place. `--target-version 0` matches every stored body,
because the articles you are trying to fix are already at the current version and
a bare run would skip them. `--domain` keeps the work to the one site the rule
can affect — on a large archive the difference is minutes against hours.

`set` prints this command for you, with the domain filled in.

That re-runs extraction **from the stored pages** — no requests to the site.
See [Reprocess the archive](reprocess-the-archive.md) for other ways to narrow
it.

### Articles that never fetched

Reprocessing cannot help an article whose page never arrived: there is nothing
stored to extract from, and a recorded fetch failure is never retried. A rule that
changes how a page is *fetched* — a user agent, a rate, a headless render — needs
those articles asked for again:

```sh
tome refetch 517626 519293        # says what it would queue
tome refetch --yes 517626 519293  # queues it
```

The ids are in **Attention**, and in the `fetch_error` column of any article listing.
`refetch` reports by default and acts only with `--yes`, because every article in the
list is a request to somebody else's server.

## Check the result

```sql
SELECT c.extractor_name, length(c.content_text) AS chars, left(c.content_text, 200)
FROM articles a
JOIN article_content c ON c.article_id = a.id AND c.is_current
WHERE a.url_canonical LIKE 'https://example.com/%'
ORDER BY c.extracted_at DESC
LIMIT 5;
```

`extractor_name` should now be `domain_rule`. If it is still `trafilatura` or
`readability`, the selector matched nothing and extraction fell through to the
heuristics — which is deliberate, because a stale rule after a site redesign
should degrade rather than return an empty article. Re-check the selector
against the stored page.

## Remove a rule

```sh
tome domain-rule rm example.com
tome reextract --target-version 0 --domain example.com
```

Worth doing when a site is redesigned and the heuristics now handle it: fewer
rules is less to maintain.

## See also

- [Extraction and versioning](../explanation/extraction-and-versioning.md) — why
  the ladder is ordered the way it is, and why rules override the ratio check
- [Reprocess the archive](reprocess-the-archive.md)
- [CLI](../reference/cli.md#tome-domain-rule)
