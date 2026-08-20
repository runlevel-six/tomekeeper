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

Ask the archive, rather than guessing from the site's reputation:

```sh
tome explain <article-id>
```

Look at the `page` row's character count — that is how much **visible text was in the
HTML that was actually served**:

| What you see | What it means |
|---|---|
| A few hundred characters or fewer | A shell. The article is built by script, and this page needs a browser. |
| Thousands of characters, but every extractor rejected it | The text is right there and the structure defeated the extractors. **A domain rule, not a browser.** |
| Thousands of characters, and a body was extracted | Nothing is wrong with this page. |

The second row is the common case, and mistaking it for the first is how a browser ends
up deployed to solve a problem a selector would have solved. On the archive this was
built against, every failing site checked was the second kind.

## 1. Scale the browser up

The manifests ship a `tomekeeper-render` Deployment at **zero replicas** — it costs
nothing until you want it:

```sh
kubectl -n tomekeeper scale deploy/tomekeeper-render --replicas=1
kubectl -n tomekeeper rollout status deploy/tomekeeper-render
```

The worker already knows where to find it (`TOME_RENDER_BROWSER_URL` points at the
Service), so there is nothing to restart. Outside Kubernetes, run the browser however
you like and set that variable yourself:

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

## When you are done

Scale it back to zero. Flagged domains then stay pending rather than failing, and pick
up again whenever a browser exists:

```sh
kubectl -n tomekeeper scale deploy/tomekeeper-render --replicas=0
```

## Troubleshooting

### Articles from a flagged domain stay `pending` forever

No browser is reachable. That is reported as a retryable condition rather than a
failure, on purpose — an operator scaling the Deployment up later gets those articles
fetched instead of finding a queue of things marked failed for a reason that has since
gone away.

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
