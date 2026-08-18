# Retention

By default nothing is ever deleted. Retention is opt-in, and this page is what
you should read before opting in.

## Turning it on

```
TOME_RETAIN_AFTER_READ=168h    # a week
```

Unset, or `0`, keeps everything forever. Anything under `1h` is rejected at
startup rather than obeyed, because the difference between `168h` and `168`
is an archive.

Off by default because the gap between the two settings is the gap between an
archive and a cache, and nobody should find out which one they are running by
noticing something is missing.

## What is protected

An article's stored copy is released only when **every** reader who can see it
has finished with it. Any one of these keeps it, for everyone:

| | |
|---|---|
| **Unread** | Never expires. This includes an article a subscriber has simply not reached yet. |
| **Starred** | Never expires. |
| **Kept** | Never expires. The ⬡ control on any article, for pages worth holding onto without having liked them. |
| **Saved by hand** | Never expires. Anything added from **Saved**. |
| **Imported** | Never expires, twice over. An imported body is `immutable` — it may be the only surviving copy of a page that is gone, so "it can be fetched again" does not hold and releasing it would lose the article, not reclaim space. Imports are also marked saved, so the rule above applies as well. |
| **Read recently** | Not until it is older than the configured window. |
| **Read, but with no recorded time** | Never expires. Possible for anything marked read before read timestamps existed; the ambiguity resolves toward keeping. |

**Starring is permanent protection, even after you unstar.** Starring stamps
`saved_at`, and unstarring deliberately leaves it — that is what keeps a starred
article reachable after the feed that introduced it is gone. The consequence is
that clearing old stars to reclaim space reclaims nothing. This is the one rule
here that surprises people.

## Why "every reader"

Bodies and images are a **shared pool**. Two people subscribed to the same site
get one archived copy between them, which is both correct and a large storage
saving — and it means one person finishing an article says nothing about whether
its bytes can go.

So expiry asks a global question: is there anybody left with a claim? Only when
the answer is no does anything get deleted. On a single-user archive this is
invisible; it is written this way so that it does not become a data-loss bug the
day a second reader exists.

## What is actually deleted

| Deleted | Kept |
|---|---|
| The extracted body, HTML and text | The article: title, URL, author, date |
| The stored copy of the original page | Everyone's read and starred state |
| Images no other article still uses | Tags and highlights |

The record survives, so search still knows the article existed, deduplication
still works, and re-fetching later re-archives it. Opening an expired article
shows when it was released and why, with the original still linked.

Images are content-addressed and shared between articles. One used by three
articles survives the expiry of two of them; only the last reference deletes it.

## What it does not cover

Feeds you have unsubscribed from, and articles nobody ever read, are not touched.
Retention releases what you have finished with — it is not a general garbage
collector.

## Watching it

The expiry pass runs hourly and logs at `INFO` every time it releases anything:

```json
{"msg":"released expired content","articles":42,"body_bytes":1104880,"asset_bytes":9437184}
```

Deleting someone's archived pages is not something that should require debug
logging to notice. `tome_body_bytes` and `tome_asset_bytes` in
[the metrics](metrics.md) show the effect on storage.

## Turning it off again

Remove the setting. Nothing further expires — but nothing already released comes
back, short of re-fetching the articles from their original URLs.

## See also

- [Configuration](configuration.md) — `TOME_RETAIN_AFTER_READ`
- [Metrics](metrics.md)
