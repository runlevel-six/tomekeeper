# Scoping and access control

There is one user today. The schema, the queries, and the tests are written for
several. This page explains why that is not premature, and what specifically is
being kept true.

## The cheap part and the expensive part

Multi-tenancy has two halves, and they cost very differently to retrofit.

Adding a `user_id` column later is a migration. Tedious, mechanical, and entirely
survivable.

Auditing every query written over two years to work out which ones needed scoping,
and which of them have been quietly returning one person's data to another, is not
survivable in the same way. There is no test that fails, no error in a log, and no
way to know you have finished. It is discovered by a family member seeing something
they should not.

So the discipline is what is protected here, not the schema.

## Every user-scoped read takes a user

`feeds`, `article_state`, `tags`, `highlights`, `feed_items`, and `import_records`
are per-user. Every repository method touching them takes a `UserID` parameter that
has no default and cannot be omitted.

That is the point of the type. `store.UserID` is a distinct type rather than an
`int64`, so passing an article id where a user id belongs does not compile. A
forgotten scope should be a build failure, not a leak found in year two.

## Some tables are deliberately shared

`articles`, `article_content`, and `assets` are a global pool.

Two people subscribed to the same site get one archived copy of each article and
one copy of each image. This is both correct — it is the same article — and a
significant storage saving, since images are most of the archive's bytes.

The consequence is a rule that has to be held to: **nothing user-specific may be
stored on a shared row.** Concretely, `articles` carries no "how did this arrive"
column and no "is this immutable" flag, because two readers can reach the same URL
by different routes and those values would have no single correct answer. Per-
reference provenance lives on `feed_items` and `import_records`; per-body
provenance lives on `article_content`. See
[Extraction and versioning](extraction-and-versioning.md).

`domain_rules` is also shared, for a different reason: how to extract a site's
articles is a technical fact about that site, identical for every reader.

## One definition of "may this reader see this article"

Because articles are shared, "which articles are mine" is a question with a real
answer that has to be computed. It is computed in exactly one place:

```sql
EXISTS (SELECT 1 FROM feed_items fi JOIN feeds f ON f.id = fi.feed_id
         WHERE fi.article_id = a.id AND f.user_id = $1)
OR EXISTS (SELECT 1 FROM article_state st
         WHERE st.article_id = a.id AND st.user_id = $1)
```

An article is visible when one of the reader's feeds references it, or when the
reader has acted on it. The second clause is what keeps a starred article reachable
after the feed that introduced it is deleted.

That predicate is a single constant in `internal/store`, embedded by the stream,
the reader, search, the attention queue, and the tag counts. Five copies of an
access rule is five chances for one to drift; one copy that is obviously wrong when
misused beats five that are subtly right.

It costs one coupling — every query embedding it must pass the user id as `$1` —
and that is a deliberate trade.

### Where it is not enough on its own

The predicate answers "may this reader see this article". It does not answer "is
this the reader's *filing*", and a filter over a shared article can leak the
second while respecting the first.

Categories are the case. A category is `feeds.category`, and two readers subscribed
to the same site legitimately see the same article through their own feed rows —
filed under whatever folder each of them used. So the category filter carries its
own scope as well:

```sql
EXISTS (SELECT 1 FROM feed_items fi JOIN feeds f ON f.id = fi.feed_id
         WHERE fi.article_id = a.id AND f.user_id = $1
           AND COALESCE(f.category, '') = $n)
```

Drop `f.user_id = $1` from that and the visibility predicate still holds — the
article really is the reader's to read — but another reader's folder names become
working filters over it. Someone could confirm what a second reader calls a feed by
finding their own article under it.

The same doubling applies to `article_tags`, which has no `user_id` of its own: the
article must be visible *and* the tag must be the reader's.

This is also a warning about tests. An isolation test built from an article only the
*other* reader can see proves nothing here, because the visibility predicate
excludes it before the filter is reached — such a test passes with the filter's own
scoping deleted. The article has to be **shared** for the assertion to have any
force. That is how the category test is written, and it fails when the clause is
removed.

## Search is not a side door

Searching `article_content` directly would work, be faster to write, and leak.

The archive is one text index over everybody's articles. A reader typing a guessed
phrase into the search box would learn whether anyone had archived a page
containing it. So search joins through the same visibility predicate as every other
read. It is the same query shape either way, so there was never a reason to do it
the other way.

## Not found, never forbidden

An article a reader may not see is reported as missing, not as refused.

"Forbidden" confirms the article exists. For a private archive that is the whole
disclosure: whether a particular URL has been saved is the interesting fact, not
its contents. The same applies to feeds, and to writes — attempting to star
somebody else's article gets the same answer as starring one that was never there.

Writes are guarded by the predicate too, not only reads. Allowing a state row
against an arbitrary article id would let a reader confirm what exists one insert
at a time.

## The test that has to pass before it matters

There is a test that creates two users with separate feeds and asserts that
neither one's stream, reader, search, feed list, tag list, unread counts, or
attention queue surfaces the other's articles.

It passes now, while there is one real user and nothing to isolate. That is the
only time it can be written cheaply, and the only time its passing means the
discipline was kept rather than reconstructed.

Its value was confirmed by deliberately weakening the visibility predicate to
`(true)`: five of those tests fail. A test on an access boundary that cannot be
shown to fail is decoration.

Do that to every scoping clause, not just the shared one. The first version of the
category isolation test passed with the category filter's `user_id` deleted, and
looked like coverage while providing none — see the warning above.

## What is deliberately not built

No signup, no invite flow, no password reset, no admin interface, no roles. There
is one user, created from configuration.

If the discipline above has been kept, adding those is a weekend of interface and
authentication work against a data layer that is already correct. If it has not
been kept, it is a rewrite — which is why this is written down as a principle
rather than scheduled as a task.

## See also

- [Why articles are the root entity](why-articles-are-the-root-entity.md) — why
  articles are shared in the first place
- [Extraction and versioning](extraction-and-versioning.md) — where per-body
  provenance lives, and why not on the article
- [Data model](../reference/data-model.md) — which columns exist on which table
