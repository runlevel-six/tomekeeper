# Why articles are the root entity

Most feed readers are built around the feed item. A feed has items; an item has
a title, a link, and a summary; the reader shows you items. It is the obvious
model, it matches the file format, and it is wrong for an archive.

Tomekeeper inverts it. The **article** is the root entity, and a feed item is
one *reference* to an article. So is a manual save. So is an imported Wallabag
entry.

## What goes wrong with feed items as the root

Say you subscribe to a site's own feed, an aggregator that syndicates it, and a
newsletter that links to it. All three carry the same story.

With items as the root, you have three rows. Three fetches of the same page.
Three extractions. Three copies of the same eight images — and images are about
80% of the archive's bytes. Mark it read in one place and it is still unread in
the other two.

Then the aggregator changes its GUID scheme, and you have six.

## What the inversion buys

An article is identified by its canonical URL, which is unique. The three
references collapse onto one row, and everything downstream follows:

- **One fetch, one extraction, one set of images.** The storage win is real and
  it compounds over a decade.
- **Read state is per-user, per-article.** Marking a story read marks it read
  everywhere it appears, because there is only one it.
- **Importers do not need a fake feed.** A Wallabag entry references an article
  directly. Without this, importing ten thousand saved pages would mean
  inventing a synthetic feed to hang them from, and that synthetic feed would
  then show up in the UI forever.
- **An article can outlive every reference to it.** Unsubscribe from the feed
  and the archived article stays, which is the whole point of an archive.

## What it costs

Deduplication is only as good as URL canonicalization, and canonicalization is
a heuristic. `internal/urlcanon` is therefore the most carefully tested pure
function in the codebase: a golden corpus in `testdata/urls/`, an idempotency
property, and a fuzz target that runs in CI.

The two ways to be wrong are not symmetrical.

**Too permissive** — failing to strip `?utm_source=` — costs you a duplicate.
Annoying, visible, fixable later by adding a rule and re-running deduplication.

**Too aggressive** — stripping `?p=123`, which is how a great many content
management systems address articles — merges two genuinely different articles
into one row. That is data loss, it is silent, and it is not recoverable once
the second article's reference has been discarded.

So the rules only strip parameters known to be tracking noise, and anything
unrecognized is preserved. The corpus documents both directions, and
`TestDistinctArticlesStayDistinct` exists specifically to catch a future
over-eager rule.

Fuzzing found two real bugs in the first minute, both of them idempotency
failures — a URL that canonicalized to something that canonicalized to
something else. Those are exactly the bugs that would produce duplicate rows
weeks later, at which point nobody would connect the two.

## Where the seam is

The article row is shared by every user. Anything specific to one person hangs
off the reference instead:

| Question | Answered by |
|---|---|
| What is this article? | `articles`, `article_content` — shared |
| Which of my feeds carried it? | `feed_items` — scoped |
| Have I read it? | `article_state` — scoped |
| Did I save or import it myself? | `article_state.saved_at`, `import_records` — scoped |

This is also why `articles` has no `origin` column. Provenance is a property of
the *reference*, not of the article: your import and a family member's
subscription can arrive at the same URL, and a single `origin` value on the
shared row would have no correct answer.

The practical consequence for anyone writing queries: **user-facing queries
must reach articles by joining through `feed_items` or `article_state`.**
Selecting from `articles` directly returns the whole household's reading. That
is why search is specified to join, from the first version, even though there
is only one user today — the query shape is identical either way, so there is
no reason to write the one that becomes a leak in M9.

## See also

- [Data model](../reference/data-model.md) — the tables and their constraints
- `internal/urlcanon/testdata/urls/canonical.txt` — the canonicalization
  specification, as executable cases
