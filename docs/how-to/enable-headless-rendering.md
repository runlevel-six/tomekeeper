# How to enable headless rendering

A few sites send an empty shell and build the article in JavaScript. Fetching one of
those stores the shell: a `<div id="app">` and nothing to read. This is how to point a
real browser at them.

**Try a domain rule first.** Rendering is the expensive answer, and most pages that
extract badly are not JavaScript pages at all — they are ordinary HTML with an unusual
structure, and a CSS selector fixes them for good. See
[Add a domain rule](add-a-domain-rule.md). The section below tells the two apart in one
command.

## Is this site actually JavaScript-rendered?

**Look at Attention.** Every row there now carries a *The page itself* column showing how
much visible text the served HTML actually contained, which is the number that answers
this:

| What it says | What it means |
|---|---|
| `shell · 412 chars` | The article is built by JavaScript. No selector can find what was never sent. **This wants a browser.** |
| `18,204 chars of text` | The text is in the HTML and the extractors could not find it. **This wants a [domain rule](add-a-domain-rule.md).** |
| `not measured` | This article has not been extracted since the measurement existed. Re-extract it to find out. |

The second row is the common case, and mistaking it for the first is how a browser ends up
deployed to solve a problem a selector would have solved.

From the command line, `tome explain <article-id>` reports the same number in its `page`
row, alongside what every rung of the ladder decided and why.

On the archive this feature was built against, **every failing site checked was the
second kind** — a structure problem, not a JavaScript one. Check before deploying a
browser at a problem a selector solves permanently.

## 1. The browser is already running

The manifests ship a `tomekeeper-render` Deployment at **one replica**, so on Kubernetes
there is nothing to do here. That is a deliberate choice to spend about 256Mi on a
browser most archives never use, because the alternative is worse: with it scaled to
zero, ticking "this site needs JavaScript" does nothing observable, and the person who
ticked it cannot tell why. Multi-user widens that gap — a reader flags the domain and only
an administrator can scale a Deployment — and it catches administrators too, inheriting a
deployment where the feature works on one installation and not on theirs.

If you would rather not spend the memory, turn it off deliberately:

```sh
kubectl -n tomekeeper scale deploy/tomekeeper-render --replicas=0
```

Flagged articles then **wait** rather than fail: they stay retryable and say
`waiting for a headless browser` in [Attention](../reference/cli.md#web-interface), so the
state is visible instead of silent. Scaling back up collects them.

Outside Kubernetes, run the browser however you like and point the worker at it:

```sh
docker run -d --name headless -p 9222:9222 chromedp/headless-shell:latest
export TOME_RENDER_BROWSER_URL=ws://localhost:9222
```

A hostname and port is enough. The websocket address a browser advertises carries a GUID
that changes every time it starts, and the client asks for the current one — so nothing
has to be reconfigured when the pod restarts.

**Do not pass arguments to that image.** It appends them to its own command line, and it
runs the browser behind a socat forwarder; a second `--remote-debugging-port` makes the
browser take the forwarder's port and nothing answers at all.

## 2. Flag the domain

Nothing is rendered until you say which sites need it. A running browser on its own
changes nothing:

```sh
tome domain-rule set --requires-js example.com
```

Or tick **Requires JavaScript** on the domain rules page. The flag follows the same
subdomain rule as everything else there: `example.com` covers `blog.example.com`.

## 3. Re-fetch what was already stored

Flagging a domain does not revisit the pages already in the archive — they have a stored
copy, and the fetcher does not re-fetch what it has. The shells have to be cleared
first:

```sql
-- The articles from that host whose extraction produced nothing.
UPDATE articles SET fetch_status = 'pending', fetch_error = NULL
WHERE fetch_status = 'failed'
  AND url_canonical LIKE 'https://example.com/%';
```

The scheduler picks up anything at `pending` within a minute and routes it to the
browser this time. Watch it happen:

```sh
kubectl -n tomekeeper logs deploy/tomekeeper-worker -f | grep -E "render|handed"
```

A successful render logs the page's size and, more usefully, how many subresource
requests it refused and allowed:

```
rendered article article_id=1487 bytes=214018 subresources_blocked=23 subresources_allowed=4
```

## What rendering costs, and what it does to other people

Worth reading once, because this is the only part of the archive that is not a polite
HTTP client.

A browser loading a page fires requests at every host the page references — advertising,
analytics, fonts, third-party script — and none of those pass through this archive's rate
limiter or robots.txt cache. Three things are done about it:

- **Images, media and fonts are refused**, by resource type rather than by file
  extension, so a CDN image with no extension in its URL is caught too. Extraction reads
  the DOM and never looks at a pixel, and the archive fetches the images it wants
  afterwards through the polite client. This is also most of the memory saving.
- **The User-Agent is the archive's**, the same string a plain fetch sends, contact URL
  included. A site that wants to know who is asking gets a truthful answer.
- **robots.txt is checked before the browser is started**, using the same cache the
  ordinary fetch path uses, and the host's rate limit is waited on. A disallowed page is
  recorded as skipped exactly as it would be otherwise.

**What cannot be fixed: the page's own JavaScript runs.** That is what rendering is.
Scripts execute, and some of them report back to whoever wrote them. This is a
deliberate exception to the archive's politeness rules, confined to the handful of
domains you flag by hand — which is the strongest argument for flagging as few as
possible.

Memory is the other cost: about a gigabyte for the pod, one page at a time
(`TOME_RENDER_CONCURRENCY`, default 1). Raising the concurrency means raising the pod's
limit with it, the same coupling image transcoding has.

## Troubleshooting

### Articles from a flagged domain say `waiting`

No browser is reachable. That is a retryable condition rather than a failure, on purpose:
scaling the Deployment up later collects those articles instead of leaving a queue of
things marked failed for a reason that has since gone away. They appear in **Attention**
with the reason, and in the reading list with a `waiting` badge rather than `queued`.

```sh
kubectl -n tomekeeper get deploy tomekeeper-render          # is it scaled up?
kubectl -n tomekeeper logs deploy/tomekeeper-worker | grep -i browser
```

`tome worker` says at startup whether a browser is configured at all.

### Nothing is being rendered, and nothing is pending either

The domain is not flagged, or it is flagged under a different host than the articles
use. `tome domain-rule list` shows the rules and how many articles each one covers;
check the count is not zero.

### The render succeeds but extraction still produces nothing

Then it was never a JavaScript problem, or not only one. The stored page is now the
rendered DOM, so `tome explain` is looking at what the browser built — and if the text
is there and the extractors still reject it, this wants a selector:
[Add a domain rule](add-a-domain-rule.md). Both can apply to one host; a rule with
`--requires-js` and a `--selector` renders the page and then extracts by hand.

### The pod is being OOM-killed

One page at a time is the default. If you raised `TOME_RENDER_CONCURRENCY`, raise the
Deployment's memory limit with it, or put it back. A render that dies takes its own
article's fetch with it and nothing else — the render queue is separate from the pool
that polls feeds precisely so that a wedged browser cannot stop the archive.

## See also

- [Add a domain rule](add-a-domain-rule.md) — the cheaper answer, and usually the right one
- [Configuration](../reference/configuration.md) — `TOME_RENDER_BROWSER_URL`, `TOME_RENDER_CONCURRENCY`
- [Non-goals](../explanation/non-goals.md) — why this is the last resort rather than the default
