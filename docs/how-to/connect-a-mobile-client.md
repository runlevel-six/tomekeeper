# How to connect a mobile client

Tomekeeper speaks the Fever sync protocol, so most third-party RSS clients can read
the archive without knowing anything about it. This is how to point one at it, and
what to expect once it is there.

The protocol itself — every argument, every field, every deviation — is in
[the Fever API reference](../reference/fever-api.md). This page is the setup.

## Before you start

You need three things:

- **The archive reachable from the phone.** A client on a mobile network cannot reach
  a service on your LAN. If Tomekeeper is only on the local network, the client only
  syncs at home.
- **HTTPS, in practice.** Several clients refuse a plain-HTTP account outright, and
  iOS requires an exception to allow one. If you are behind the Ingress from
  [How to install on Kubernetes](install-kubernetes.md), this is already true.
- **A password set.** The Fever credential is derived from it, and it does not exist
  until a password does — see below.

## Check the API answers

Do this first, from a machine with a shell. It takes one command and it separates "the
client is misconfigured" from "the archive is not answering", which is otherwise a
long evening.

```sh
API_KEY=$(printf '%s:%s' "$TOME_USERNAME" "$TOME_PASSWORD" | md5sum | cut -d' ' -f1)

curl -s -X POST "https://tomekeeper.example/fever/?api&feeds" -d "api_key=$API_KEY" | jq .
```

A working archive answers with `"auth": 1` and your subscriptions:

```json
{
  "api_version": 3,
  "auth": 1,
  "last_refreshed_on_time": 1755712345,
  "feeds": [
    { "id": 1, "favicon_id": 0, "title": "Ars Technica", "url": "…", "is_spark": 0, … }
  ],
  "feeds_groups": [ { "group_id": 1442081, "feed_ids": "1,7,12" } ]
}
```

`"auth": 0` means the key was not accepted. Note that this comes back as **HTTP 200** —
the protocol reports its result inside the body, so `curl -f` will not flag it.

## Set the account up in the client

Choose the **Fever** account type. Then:

| Field | Value |
|---|---|
| Server / URL | `https://tomekeeper.example/fever/` |
| Username / email | your `TOME_USERNAME` |
| Password | your password |

The client computes the `api_key` itself; you never type it.

**If the client rejects the URL, try it without the `/fever/` suffix**, or with it
added. Clients disagree about whether they append it, and there is no way to tell from
the outside which a given one does. Both spellings work here — `POST /fever` and
`POST /fever/` are both routed — so the failure is only ever a client that built a
path this service does not have, such as `…/fever/fever/`.

Some clients label the username field "email" because Fever accounts were email
addresses. Put the username in it regardless.

## What works, and what a client cannot do

| | |
|---|---|
| Reading, with the **full extracted body** rather than the feed's summary | ✅ |
| Read and unread, syncing both ways | ✅ |
| Starring — the client calls it saved | ✅ |
| Folders, from the categories your feeds are filed under | ✅ |
| Mark a feed, a folder, or everything read | ✅ |
| Archived images | ✅ |
| Subscribing, unsubscribing, renaming, refiling | ❌ Use the web interface. |
| Search | ❌ Not in the protocol. Use the web interface. |
| Tags, highlights, the reading list, retention, domain rules | ❌ Not in the protocol. |
| Favicons and Fever's "Hot links" | ❌ Not implemented; see the reference. |

Subscribing is not a gap in this implementation — the protocol never had it. Its own
documentation deferred feed management to an update that never shipped.

## Verify it end to end

Worth doing once, because a client that syncs but does not write is a failure you
otherwise discover weeks later when the unread count on your phone disagrees with the
one in the browser.

1. In the client, open an article and read it.
2. Reload the web interface. It should be read there too.
3. Star something in the client. It should show as starred in the browser.
4. Mark an article unread in the browser. The client should agree at its next sync.

## Troubleshooting

### `"auth": 0`, or the client says the credentials are wrong

**Has a password ever been set?** The Fever key is MD5 of `username:password`, which
cannot be recovered from the stored argon2id hash — so it is written *when the password
is set* and does not exist before. If sign-in to the web interface also fails, run
`tome migrate` with `TOME_PASSWORD` set.

**Was the password changed?** Changing it rotates the Fever key and disconnects every
client. This is not a bug: the key is the credential. Re-enter the password in each
client.

**Is the username right?** It is `TOME_USERNAME`, which defaults to `tome` and is not
an email address even though the client may ask for one.

The server log has one line per rejected attempt, with the remote address and user
agent, which is how to tell a client that never arrived from one whose key is wrong.

### Articles arrive but the pictures do not

Images in a synced body are absolute URLs carrying a signature, because a client has no
session cookie to fetch them with. Three things break them:

- **`TOME_SESSION_KEY` changed or was never set.** The signing key is derived from it.
  If it is unset, one is generated per process, so every restart invalidates every
  image URL already synced — the log says so at startup. Set it.
- **The URLs are older than 30 days.** They expire. The client re-fetching the body
  gets fresh ones.
- **The scheme is wrong.** The URLs use `https` unless `TOME_COOKIE_SECURE` is false.
  A deployment serving plain HTTP with `TOME_COOKIE_SECURE=true` produces `https` URLs
  that nothing answers.

### An article shows one sentence saying there is no stored copy

That is the archive being honest: extraction produced nothing for that page, or
retention released the body. `GET /attention` in the web interface lists these with the
reason, and [How to add a domain rule](add-a-domain-rule.md) is usually the fix.
Image-only webcomics are the common case and cannot be fixed — there is no article text
to extract.

### Only 50 articles arrive

That is the protocol's page size. The client asks again with `since_id` or `max_id`
until it has everything; a first sync of a large archive is many requests. If it stops
at 50 permanently, the client is not paging — which is a client bug, and worth trying
another one before suspecting the server.

### An article the client never showed me is marked read

A bulk mark carries a `before` timestamp so that this cannot happen. If a client omits
it, the mark applies to the whole list — see
[`before` in the reference](../reference/fever-api.md#before). Nothing is lost: mark it
unread again, in either place.

### A folder appeared, or a folder's contents changed

Folders are your feed categories, and a folder's id is derived from its name. Renaming
a category therefore looks to a client like the old folder going away and a new one
arriving. Refiling a feed in the web interface moves it in the client at the next sync.

## Two clients, on purpose

If you are validating this rather than just using it, use two. The milestone's own
acceptance criterion asks for two independent clients because they disagree about the
URL, about whether they send `before`, and about how they page — and a bug in any of
those is invisible against a single client that happens to avoid it.
