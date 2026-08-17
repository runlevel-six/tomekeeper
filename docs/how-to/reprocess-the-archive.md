# How to reprocess the archive

`tome reextract` re-runs extraction over articles already stored, using the raw
pages kept at fetch time. It makes no requests to any site.

Reach for it after changing anything that affects extraction: adding a domain
rule, upgrading an extractor library, or taking a release that bumped the
extractor version.

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
| `--since-version V` | Select articles whose body came from a version other than `V`. |
| `--dry-run` | Count without queueing. |

To reprocess **everything**, including articles already at the current version
— which is what you want after adding a domain rule, since those articles are
at the current version and would otherwise be skipped:

```sh
tome reextract --since-version 0
```

Version `0` never matches any stored body, so every mutable article qualifies.

To try a change on a hundred articles before committing to the whole archive:

```sh
tome reextract --since-version 0 --limit 100
```

Then compare before and after:

```sql
SELECT extractor_name, count(*), round(avg(length(content_text))) AS avg_chars
FROM article_content
WHERE is_current
GROUP BY extractor_name
ORDER BY count(*) DESC;
```

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
- [CLI](../reference/cli.md#tome-reextract)
