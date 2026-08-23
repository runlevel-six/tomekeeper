# How to reprocess the archive

> **Since 2026-08-21 this also reaches articles that produced *no* body.** It did not
> before, and that was the most expensive bug in the feature: candidates were selected by
> comparing the version that produced an article's body, so an article with no body could
> not be selected — and every extraction improvement silently skipped exactly the articles
> improvements exist to rescue. Found by measuring: 343 articles with a stored page and no
> body, 280 of them webcomics from one host, which the image rung added three versions
> earlier would have archived. A one-off catch-up run after upgrading past that release
> will pick all of them up.


Re-extraction re-runs extraction over articles already stored, using the raw
pages kept at fetch time. It makes no requests to any site.

Reach for it after changing anything that affects extraction: adding a domain
rule, upgrading an extractor library, or taking a release that bumped the
extractor version.

## From the interface

**`/reprocess`** is the whole-archive form, linked from Settings and from the foot of
the rules page. Nothing here needs a terminal.

It counts before it does anything, and it counts two things separately, because they
answer different questions:

| The offer | Selects | Reach for it |
|---|---|---|
| Bring *N* up to date | Bodies from an extractor version other than this build's | After an upgrade that changed how extraction works |
| Re-extract all *N* again | Every mutable body in that slot, whatever version | After editing rules — a rule is data rather than a version, so a body it would now change is not "out of date" by any number |

**Whose bodies move is the other axis, and the page keeps the two apart.** Your own
extractions are the ones your rules produced, and redoing them affects nobody else. The
household's are what every reader sees unless they hold their own — so that section is
offered to administrators only, and refused to anybody else rather than quietly
downgraded to their own bodies.

Holding no bodies of your own is the ordinary state and the page says so rather than
offering a button over a count of zero: copy-on-write means you get a body of your own
only where [a rule of yours](add-a-domain-rule.md) produces something different from the
household's extraction.

Reprocessing **one host** is on that host's own row on the rules page, which is the
usual case. The row decides whose bodies move, and the two are different questions:
on your own rule's row it applies your rules to that host, including articles you have
no body of yet; on the household's row an administrator brings everybody's forward.

The rest of this page is the command, which is what an operator wants for a scripted or
partial run.

## See what would happen first

```sh
tome reextract --dry-run
```

```
1,284 articles would be re-extracted (dry run, nothing queued)
```

By default this selects everything whose current body came from an extractor
version other than the one now compiled in. Two kinds of article are never
selected:

- **Imported bodies.** Excluded by the query, not skipped in a loop — a
  Wallabag entry may be the only surviving copy of a dead URL, and it is never
  regenerated.
- **Articles with no stored page.** There is nothing to re-extract from.

## Queue the work

```sh
tome reextract
```

```
queued 1,284 articles for re-extraction; run `tome worker` to process them
```

The command only queues; the worker does the work, at the concurrency the
worker is configured for. That means a reprocess of the whole archive competes
with normal polling instead of monopolizing the machine, and it survives a
restart because the queue is in Postgres.

Watch it drain:

```sql
SELECT state, count(*) FROM river_job
WHERE kind = 'extract_article'
GROUP BY state;
```

## Narrow it down

| Flag | Use |
|---|---|
| `--limit N` | Queue at most N articles. Good for trying a change on a sample first. |
| `--target-version V` | Select articles whose body came from a version other than `V`. |
| `--domain` | Restrict to one host and its subdomains. |
| `--dry-run` | Count without queueing. |

To reprocess **everything**, including articles already at the current version
— which is what you want after adding a domain rule, since those articles are
at the current version and would otherwise be skipped:

```sh
tome reextract --target-version 0
```

Version `0` never matches any stored body, so every mutable article qualifies.

To reprocess **one site** — the usual case, after writing a domain rule for it:

```sh
tome reextract --target-version 0 --domain example.com
```

That covers subdomains too, so `example.com` reaches `blog.example.com`, matching
how the rule itself applies. It compares the article's host rather than searching
the URL, so `notexample.com` is not swept in and neither is a link that merely
mentions the domain in a query parameter.

To try a change on a hundred articles before committing to the whole archive:

```sh
tome reextract --target-version 0 --limit 100
```

Then compare before and after:

```sql
SELECT extractor_name, count(*), round(avg(length(content_text))) AS avg_chars
FROM article_content
WHERE is_current
GROUP BY extractor_name
ORDER BY count(*) DESC;
```

## When a handful will not go away

A reprocess of the whole archive usually leaves a few articles behind, and
`--dry-run` keeps reporting the same small number afterwards. That is not a bug in
the queue: an article that fails extraction writes no body, so it still has nothing
from the current version and is selected again on the next run. It will keep being
selected until it either extracts or is left alone deliberately.

Find them:

```sql
SELECT id, url_canonical, fetch_status, fetch_error
FROM articles a
WHERE NOT EXISTS (
        SELECT 1 FROM article_content c
        WHERE c.article_id = a.id AND c.is_current)
  AND a.raw_blob_path IS NOT NULL
ORDER BY id;
```

Then ask why, one at a time:

```sh
tome explain <id>
```

That reports what every rung of the ladder produced from the page already stored
and which threshold turned it down — no requests, and an answer even for a site
that has since changed. Most of the time it is one of two things: a page with no
article in it (a JavaScript shell or a consent wall, which no rule can fix), or a
body the heuristics cannot find, which is what [a domain
rule](add-a-domain-rule.md) is for.

## What reprocessing does not lose

Re-extraction never destroys the previous body. The old row stays with
`is_current = false` and the new one becomes current, so a regression can be
diagnosed by comparing what the same page produced before:

```sql
SELECT extractor_name, extractor_version, is_current, extracted_at,
       length(content_text) AS chars
FROM article_content
WHERE article_id = $1
ORDER BY extracted_at DESC;
```

If a change made things worse, fix the cause and re-run. The raw page is still
there; nothing has been consumed.

## Storage

Every reprocess adds a row per article. Bodies are text and compress well in
Postgres, but a decade of monthly reprocessing is a decade of rows. To reclaim
space by dropping superseded bodies — after you are satisfied with the current
ones:

```sql
DELETE FROM article_content
WHERE NOT is_current
  AND NOT immutable
  AND extracted_at < now() - interval '90 days';
```

Keep `NOT immutable` in that statement. It is the same protection the reprocess
itself relies on.

## See also

- [Add a domain rule](add-a-domain-rule.md)
- [Extraction and versioning](../explanation/extraction-and-versioning.md) — why
  the extractor version exists and when to bump it
- [CLI](../reference/cli.md#tome-reextract), and
  [`tome explain`](../reference/cli.md#tome-explain) for one article at a time
