# Fever API

The sync protocol `tome serve` speaks to mobile RSS clients. It exists so that
reading this archive on a phone means using a client you already have rather than
waiting for an app that will never be written.

Fever was a hosted reader, discontinued around 2016, whose API a generation of iOS
and Android clients implemented. The specification is frozen — its own text promises
that existing features "will not be removed or modified" — so the wire format
described here is final, and this page records what is implemented, what is not, and
where this implementation deliberately differs.

Clients known to speak it include Reeder, Unread, Fiery Feeds, Fluent Reader, Read
You, and NetNewsWire's Fever account type. See
[How to connect a mobile client](../how-to/connect-a-mobile-client.md) for setting
one up.

## Endpoint

```
POST /fever/
```

`POST /fever` without the trailing slash is registered too, so a client that omits it
is not answered with a redirect it may not repeat the body for.

Read arguments go in the **query string**, write arguments in the **POST body**, and
the credential in either. This split is the protocol's, not a preference: a client
that posts a read argument is asking for something Fever itself would not have
recognized.

Every response is `application/json; charset=utf-8` with `Cache-Control: no-store`.

## Authentication

`api_key` is the MD5 checksum of `username:password` — the account's username as
`TOME_USERNAME` has it, a colon, then the cleartext password.

```sh
printf '%s:%s' "$TOME_USERNAME" "$TOME_PASSWORD" | md5sum
```

MD5 is not a choice made here. It is the format every client implements, and the
value is a credential in transit rather than a stored password: the password itself is
argon2id (see [configuration](configuration.md)).

Two consequences follow from that, and both are properties of the protocol:

- **The key is written when the password is set**, not derived on demand, because MD5
  of the cleartext cannot be recovered from an argon2id hash. `users.api_key` holds
  it.
- **Changing the password rotates the key** and disconnects every connected client.
  That is correct — the key *is* the credential — but it is worth knowing before you
  change a password on a Sunday evening.

A request whose key is absent, unknown, or malformed is answered with **HTTP 200** and
a body of `{"api_version": 3, "auth": 0}` and nothing else. The status code looks
wrong and is not: this protocol carries its own result inside the body, and clients
read `auth` rather than the HTTP status. A refusal carries no other member, so an
unauthenticated request cannot learn anything about the archive.

Failed attempts are logged with the remote address and user agent. The key is never
logged.

## Response envelope

Every authenticated response carries:

| Member | Type | |
|---|---|---|
| `api_version` | integer | Always `3`, which is what Fever 1.14 reported. |
| `auth` | boolean integer | `1`. |
| `last_refreshed_on_time` | Unix timestamp | When any of this reader's feeds was last **polled**, or `0` if none ever has been. |

`last_refreshed_on_time` reads the poll attempt rather than the last successful one,
because the specification is precise about it being the most recently *refreshed*
feed, "not updated". The per-feed `last_updated_on_time` is the other one.

## Read arguments

Named in the query string. Every requested member is answered, so
`?api&groups&feeds&items` returns all of them in one response.

| Argument | Members returned |
|---|---|
| `groups` | `groups`, `feeds_groups` |
| `feeds` | `feeds`, `feeds_groups` |
| `items` | `items`, `total_items` |
| `unread_item_ids` | `unread_item_ids` |
| `saved_item_ids` | `saved_item_ids` |
| `favicons` | `favicons` — always an empty array |
| `links` | `links` — always an empty array |

### `items`

Returns at most **50** items, ordered by item id, with three optional arguments
controlling which 50:

| Argument | Selects |
|---|---|
| *none* | The oldest 50, ascending. |
| `since_id=N` | Items with an id greater than `N`, ascending. This is how a client asks what has arrived since. |
| `max_id=N` | Items with an id smaller than `N`, descending. |
| `max_id=0` | The newest 50, descending. |
| `with_ids=1,2,3` | Exactly those items, at most 50, ascending. |

`max_id=0` is a compatibility detail worth stating plainly. The specification says to
use "the lowest id of locally cached items (or 0 initially)", which taken literally
asks for items with an id below zero — so every client's first sync would return
nothing. Zero therefore means the newest page.

An unparseable or empty `with_ids` returns no items rather than everything.

`total_items` counts every article the reader can see, which is the same population
the item pages draw from — so it includes pages saved by hand, not only what their
subscriptions carry.

### `unread_item_ids` and `saved_item_ids`

A comma-separated list of ids in one string, or the empty string. **Not truncated**,
which is unusual for this application and is the protocol's requirement: a client
reconciles its cache against the whole list, so a shortened one is not a smaller
answer but a wrong one — every id past the cut would read as "no longer unread".

## Object shapes

### `item`

| Member | |
|---|---|
| `id` | The article id. See [Item ids are article ids](#item-ids-are-article-ids). |
| `feed_id` | The reader's feed that saw the article first, or `0` if none carries it. |
| `title`, `author`, `url` | As stored. `url` is the canonical URL. |
| `html` | The extracted body. See [Bodies and images](#bodies-and-images). |
| `is_read`, `is_saved` | Boolean integers. `is_saved` is the starred flag. |
| `created_on_time` | Publication date where the feed gave one, arrival otherwise — the same expression the web interface orders its streams by, so a client's ordering agrees with the archive's. |

### `feed`

| Member | |
|---|---|
| `id`, `title`, `url`, `site_url` | The subscription. `url` is the feed address. |
| `favicon_id` | Always `0`. Favicons are not stored. |
| `is_spark` | Always `0`. Sparks were Fever's low-priority feeds, a distinction this archive does not make. |
| `last_updated_on_time` | When the feed last successfully brought content, or `0` if never. |

Disabled feeds are listed. A feed that has stopped being polled still has articles in
the archive, and a client that could not see the feed would show those articles as
belonging to nothing.

### `group` and `feeds_group`

| Member | |
|---|---|
| `group.id` | A positive integer derived from the category name. See below. |
| `group.title` | The category name. |
| `feeds_group.group_id` | The group. |
| `feeds_group.feed_ids` | Comma-separated feed ids in one string, which is the protocol's own choice of shape. |

Groups are sorted by title.

## Write arguments

Named in the POST body.

| Request | |
|---|---|
| `mark=item&as=read&id=N` | Mark one article read. |
| `mark=item&as=unread&id=N` | Mark it unread. |
| `mark=item&as=saved&id=N` | Star it. |
| `mark=item&as=unsaved&id=N` | Unstar it. |
| `mark=feed&as=read&id=N&before=T` | Mark one feed's articles read. |
| `mark=group&as=read&id=N&before=T` | Mark one category's articles read. |
| `mark=group&as=read&id=0&before=T` | Mark everything read — Fever's "Kindling" super group. |
| `mark=group&as=read&id=-1&before=T` | Fever's "Sparks" super group. Always empty here, so this does nothing. |

`as=unread` is not in the specification's list for `mark=item`, which names only
`read`, `saved` and `unsaved`. Clients send it, and it is the only way a reader can
undo a mistaken tap, so it is accepted.

A mark response always carries `unread_item_ids` and `saved_item_ids`, whether or not
they were requested. This is what the specification means by returning them "as
appropriate", and it is what keeps a client's cache correct after a write it did not
follow with a read.

An unrecognized `as` value changes nothing and is logged. An `id` naming an article,
feed or group the reader cannot see changes nothing and is reported as success —
distinguishing it would let a client discover what exists in somebody else's archive
one request at a time.

### `before`

`before` is a whole-second Unix timestamp, and its job is to stop a bulk mark reaching
items the client has never shown anybody: a client sends the time of its most recent
`items` request, and articles that arrived after it stay unread.

- **Omitted, zero or unparseable means no bound**, so the mark applies to the list as
  it stands. That is what the web interface's own "mark all as read" does, and it is
  what makes a client's button work rather than silently doing nothing.
- The comparison is strict and against the item's `created_on_time`. An article whose
  timestamp falls in the same second as `before` is therefore *excluded*. Second
  resolution is the protocol's; excluding is the conservative side of it.

## The mappings

Four places where Fever's model and this archive's do not line up. Each is a decision
with a consequence.

### Item ids are article ids

In Fever an item belongs to exactly one feed. Here the article is the root entity and
a feed reference is one of several ways to reach it, so a story carried by three
subscriptions is one article — see
[why articles are the root entity](../explanation/why-articles-are-the-root-entity.md).

So an item id is an article id, and a syndicated story appears once. The alternative,
a per-reference id, would show it once per subscription and would leave pages saved by
hand unrepresentable, since those have no feed reference at all — which would make
`saved_item_ids` able to name items that `items` could never return.

Two consequences:

- **`feed_id` is `0`** for an article no subscription of theirs carries, which is the
  ordinary case for a page saved by hand. Clients generally file these under "all
  items".
- **`since_id` can miss an article.** Ids ascend with arrival, so an article already
  in the archive that a newly added subscription starts carrying keeps its old id, and
  a client polling with `since_id` will not see it. It arrives via
  `unread_item_ids`, which is the mechanism clients use to reconcile.

### Groups are categories

`feeds.category` is free text on a subscription; there is no categories table and so
no id to hand out, which the protocol requires. The group id is therefore derived from
the category name by hashing it into `[1, 2^31-1]`.

Deriving it from the name rather than from position is what makes it **stable**: a
client caches its folder list and each folder's membership against these numbers, and
an id that moved when an unrelated category was added would silently reshuffle
somebody's reader. Renaming a category changes its id, which a client sees as a new
folder — correct, since the title changed too.

Feeds with no category appear in **no group**. Fever has no concept of an ungrouped
feed and clients cope with it; inventing an "Uncategorized" group would put a folder
in somebody's reader that does not exist in their archive.

### `is_saved` is starred, not saved

Fever has one flag where this archive has two. Starring is a reaction to something a
feed brought; `article_state.saved_at` additionally records the moment a page was
archived — which starring also sets, and unstarring deliberately does not clear, so
that an article stays reachable after the feed that introduced it is gone.

That makes `saved_at` one-directional and therefore unusable as the mapping: a client
unsaving an item could never see it take effect. So `is_saved` is the starred flag,
and `as=saved` stars. The archive's own **Saved** reading list is a different thing,
and its pages appear as ordinary items.

### Bodies and images

`html` carries the **extracted body**, not the summary the feed shipped. That is the
whole point of the milestone: a truncated feed is the problem this archive exists to
solve, and a client that shows two sentences and a "read more" has not been given
anything.

Image sources in that body are rewritten to **absolute, signed URLs**:

```
https://tomekeeper.example/assets/sha256/a1/b2/….avif?sig=<expiry>.<mac>
```

`/assets/` requires a session, and a client authenticating with an `api_key` in a POST
body has no cookie an `<img>` tag could carry — so without the signature every picture
in every client is a broken image icon. The signature is the credential, which is the
same answer Miniflux reaches with its media proxy.

- The MAC covers the path and the expiry together, so a signature cannot be moved to
  another image and an expiry cannot be extended.
- The key is derived from `TOME_SESSION_KEY` with its own HKDF label, so it is
  independent of the session cipher. **Rotating that secret invalidates outstanding
  image URLs** along with every session, and a client will re-fetch bodies to get
  fresh ones.
- URLs are valid for **30 days**, long enough to read what you synced.
- The URL carries one query parameter rather than two, so that HTML escaping cannot
  touch it — an `&` between parameters serializes as `&amp;`, and whether the image
  then loads would depend on the client's HTML parser rather than on this service.

The host comes from the request, so the URLs work over whichever hostname the client
reached. The scheme follows `TOME_COOKIE_SECURE` when TLS is terminated upstream,
which is every deployment behind an Ingress; a deployment serving plain HTTP has to
have said so already for its session cookie to work.

An article with **no stored body** — extraction produced nothing, or retention
released it — gets one sentence saying so and a link to the original, rather than a
blank pane with no way onward. Image-only webcomics are the population this affects
most.

## Deviations

| | |
|---|---|
| **Writes are handled before reads, and every requested member is answered.** | The specification says a mark response carries the updated id lists, so clients legitimately combine a POSTed mark with query arguments. A server dispatching on the first match would silently drop the write and answer the read. Losing a read costs a retry; losing a write loses state. |
| **`max_id=0` means the newest page.** | Taken literally the specification's own initial-sync instruction returns nothing. |
| **`as=unread` is accepted.** | Not in the specification's list; clients send it. |
| **`before` is honored on `mark=group&id=0`.** | Miniflux ignores it for the whole-archive case. It exists precisely so a bulk mark cannot reach items nobody has been shown, and there is no reason that case should be the exception. |
| **`favicons` and `links` return empty arrays** rather than being absent. | A client that reads the member unconditionally keeps working. |

## Not implemented

| | |
|---|---|
| `links` (Hot Links) | A computed popularity ranking, which the [non-goals](../explanation/non-goals.md) rule out. Answered as an empty array. |
| `favicons` | Favicons are not stored. Answered as an empty array, with every `favicon_id` zero. |
| `unread_recently_read=1` | Would need a definition of "recently" this archive does not have. |
| `api=xml` | The XML response shape. No surviving client asks for it. A request for it is answered with JSON. |
| Adding, editing or deleting feeds | Not in the protocol either — its own text defers this to an update that never shipped. Use the web interface. |

## Limits

| | |
|---|---|
| Items per request | 50 |
| `with_ids` entries | 50 |
| Signed image URL lifetime | 30 days |
| Request body | 1MB (the server's `MaxHeaderBytes` sibling; a Fever request is small) |

## Multi-user

There is one user today, and the API is scoped as though there were not: every query
is bounded by the same visibility predicate the web interface uses, and a credential
cannot reach another reader's articles, feeds, or ids by any argument this protocol
offers. See
[scoping and access control](../explanation/scoping-and-access-control.md), and the
tests in `internal/server/fever_integration_test.go` that assert it.
