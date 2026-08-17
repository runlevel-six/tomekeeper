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

To check what the extractors currently do with the page you already have
stored, without going back to the network:

```sh
zcat /var/lib/tomekeeper/articles/2026/08/the-slug-a1b2c3/raw.html.gz | less
```

The path is in `articles.raw_blob_path`.

## Write the rule

```sh
tome domain-rule set \
  --selector 'div[data-role="story-body"]' \
  --strip '.promo' \
  --strip '.newsletter-signup' \
  --notes 'body is in a data-role attribute; promos are inline in the flow' \
  example.com
```

**The flags come before the domain.** Argument parsing stops at the first
non-flag word, so `set example.com --selector ...` treats the selector as a
stray argument and prints the usage text instead of saving anything.

| Flag | What it does |
|---|---|
| `--selector` | CSS selector for the article body. Extraction uses this instead of the heuristics. |
| `--strip` | Selector removed before extraction. Repeat for several. |
| `--rate` | Per-host requests per second, overriding `TOME_FETCH_RPS`. Use it for a site that has asked you to slow down. |
| `--requires-js` | Marks the domain as needing a headless render. Has no effect until M8. |
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
  notes:    body is in a data-role attribute; promos are inline in the flow
```

## Apply it to what is already stored

A rule changes nothing on its own. Articles already extracted keep the body
they have until they are reprocessed:

```sh
tome reextract --since-version 0   # anything not at version 0, i.e. everything
```

That re-runs extraction **from the stored pages** — no requests to the site.
See [Reprocess the archive](reprocess-the-archive.md) for narrowing it down.

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
tome reextract --since-version 0
```

Worth doing when a site is redesigned and the heuristics now handle it: fewer
rules is less to maintain.

## See also

- [Extraction and versioning](../explanation/extraction-and-versioning.md) — why
  the ladder is ordered the way it is, and why rules override the ratio check
- [Reprocess the archive](reprocess-the-archive.md)
- [CLI](../reference/cli.md#tome-domain-rule)
