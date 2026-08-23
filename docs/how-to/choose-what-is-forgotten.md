# How to choose what is forgotten

Retention is off by default and there are two halves to it: **your** reading, which
you choose in Settings, and the **archive's** default, which an operator sets. This
page is the tasks. [Retention](../reference/retention.md) is the mechanism, and worth
reading once before turning any of it on.

The one thing to hold onto: forgetting is about *your history*, not about disk space.
Your window says how long an article you have read stays on your lists and in your
reading record. The stored copy goes only when **everybody** here has finished with
it, so your choice never costs anybody else an article.

## Choose how long your reading stays yours

**Settings → Forgetting.** Five choices, and two of them are not the same thing:

| Choice | Means |
|---|---|
| Whatever the archive does | Follow the default an operator set. This is where every account starts. |
| Keep everything | Nothing of yours is ever forgotten, whatever the archive's default becomes later. |
| 30 days, 90 days, A year | After that long, an article you have read drops off your lists and the record of having read it goes. |

Round numbers rather than a text field on purpose: the difference between 45 and 60
days is not a decision anybody is really making, and a typed duration invites the
typo that deletes a year of reading.

**"Whatever the archive does" and "Keep everything" differ when the default
changes.** The first follows it; the second is a standing answer of no.

## Keep something regardless

Anything you have engaged with is never forgotten, whatever your window says:

- **Starred**, **saved**, or **kept** — any one of the three.
- **Highlighted.** An annotation is the one thing a reader may value more than the
  article, so a highlight blocks forgetting outright rather than being deleted on a
  timer with everything else.

**Engaging with an article un-forgets it.** If your window has already lapsed and you
open, star, keep or save the article, your claim comes back — otherwise the archive
would be saying "nobody wants this" about something you are looking at.

So the way to protect something is the way you would protect it anyway. There is no
separate exemption list to maintain.

## Set the default for everybody

The operator's half, and it is the *default* window rather than a deadline: a reader
who has chosen their own keeps it.

```sh
TOME_RETAIN_AFTER_READ=2160h    # 90 days
```

Unset, or `0`, keeps everything forever. Anything under `1h` is refused at startup
rather than obeyed. See [Retention](../reference/retention.md#turning-it-on) for why
it is off unless asked for.

Changing it moves every reader who is on "Whatever the archive does" and nobody else.

## Look before you turn it on

Retention deletes things, so it is worth knowing what a window would reach before
choosing one. Count what is old enough, per reader:

```sql
SELECT u.username,
       count(*) FILTER (WHERE st.read_at < now() - interval '90 days') AS older_than_90d,
       count(*) FILTER (WHERE st.read_at < now() - interval '1 year')  AS older_than_a_year
FROM article_state st
JOIN users u ON u.id = st.user_id
WHERE st.forgotten_at IS NULL
  AND st.read AND NOT st.starred AND NOT st.kept AND st.saved_at IS NULL
  AND st.read_at IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM highlights h
                  WHERE h.article_id = st.article_id AND h.user_id = st.user_id)
GROUP BY u.username;
```

That `WHERE` clause is the forgetting pass's own, which is why it is worth copying
rather than approximating: every term in it is something that protects an article,
and a shorter version of this query overstates what a window would reach.

Those counts are articles that would be **forgotten by that reader**, which is not
the same as articles that would be deleted: a stored copy survives while any other
reader still holds a claim. If you want the second number, the honest way to get it
is to set a window and read the log — see below — because "would nobody else want
this" is exactly the question the expiry pass answers and a query here would be a
second implementation of it.

## Watch it happen

The forgetting pass and the expiry pass both log at `INFO`, hourly, and only when
they do something:

```sh
kubectl -n tomekeeper logs -l app.kubernetes.io/component=worker --since=24h \
  | grep -E 'forgot old reading|released expired content'
```

```json
{"msg":"forgot old reading","user_id":1,"window":"2160h0m0s","forgotten":37,"removed":2}
{"msg":"released expired content","articles":12,"body_bytes":402240,"asset_bytes":1048576}
```

`forgotten` is claims released; `removed` is the rows deleted outright, which happens
only for an article that was reachable through that reader's state alone. `released
expired content` is the archive actually reclaiming bytes, and it is a separate line
because the two are separate decisions — a reader finishing with an article is not
the same event as the last reader finishing with it.

Deleting somebody's archived pages should not need debug logging to notice, which is
why both lines are `INFO`.

## Reclaim the space unsubscribing left

Retention only ever reaches articles somebody **read**. Unsubscribing leaves a
different residue: articles no feed references and nobody acted on. That is
`tome prune`, which reports and changes nothing until you say so:

```sh
kubectl -n tomekeeper exec deploy/tomekeeper-worker -- tome prune --list
kubectl -n tomekeeper exec deploy/tomekeeper-worker -- tome prune --yes
```

It keeps the articles themselves, and keeps anything starred, saved, read or
imported. It is an operator's command because it touches shared storage.

## Stop it

Set your own window back to **Keep everything** in Settings, or remove
`TOME_RETAIN_AFTER_READ` to stop the archive's default reaching anybody.

Nothing further is forgotten. **Nothing already released comes back**, short of
fetching those articles again from their original URLs — and for a page that has
since changed or gone, not even then. That asymmetry is the reason this is off by
default.

## See also

- [Retention](../reference/retention.md) — what expires, what never does, and why it
  waits for every reader
- [Export everything](export-everything.md) — a copy of the archive before changing
  anything that deletes
- [Back up and restore](back-up-and-restore.md)
