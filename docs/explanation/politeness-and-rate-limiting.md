# Politeness and rate limiting

This service fetches pages from servers it does not own, on a schedule, without
anyone watching. The design commits to being a polite network
citizen, and that is ethics and self-interest at the same time: an impolite
fetcher gets its address banned, and then the archive stops growing.

Every rule below exists to keep this service welcome.

## Per-host rate limiting is the one that matters

Requests are limited by a token bucket **per host**, defaulting to one request
per second with a burst of three.

A global limit — "no more than ten requests in flight" — protects this machine
from doing too much work at once. It limits nothing that any individual server
experiences, because a server only ever sees the traffic sent to *it*. Ten
concurrent requests spread across ten hosts is polite; ten concurrent requests
to one host is a small denial-of-service attempt.

Both limits exist, for their different reasons. The per-host bucket is the one
that keeps you off blocklists.

A burst of three rather than one is deliberate: a strict burst of one would
serialize even the first two requests to a host untouched for an hour, which is
needlessly slow and is not something any server would have noticed.

`TOME_FETCH_RPS` sets the default. A domain rule's `--rate` overrides it for one
host — either direction, for a site that has asked for less or one that is
comfortable with more.

## robots.txt, and one deliberate exception

`robots.txt` is fetched once per host, cached for a day, and respected for
**article and asset fetches**.

**Feed polls are exempt.** A feed is a subscription the reader explicitly asked
for, published in a format whose entire purpose is automated consumption.
Refusing to poll it because the site's `robots.txt` disallows crawlers would
break the thing the reader set up, on the strength of a rule aimed at search
engines. Article fetches get no such exemption: those URLs were discovered
rather than chosen, which is exactly the situation `robots.txt` is about.

A disallowed article is recorded as `skipped`, not `failed`. The site said no;
that answer will not change on retry, and keeping the two states apart is what
keeps the failed-fetch queue a list of things worth looking at.

### When robots.txt cannot be read

A `4xx` means no restrictions, which every reading of the specification agrees
on. A `5xx` is where this deviates: Google's crawler specification treats a
server error as a full disallow, and this service treats it as permissive.

The reasoning is about what each is doing. A crawler discovering the open web
should back off when it cannot read the rules. This service fetches pages a
specific person deliberately subscribed to, and halting a personal archive
because a host's `robots.txt` is briefly returning 500 enforces a restriction
the site never actually expressed.

The implementation calls `TestAgent` rather than `FindGroup` followed by `Test`.
That is not a stylistic choice: the allow-all and disallow-all states a
`robots.txt` can be in are only honored by `TestAgent`, while `FindGroup`
returns an empty group for them whose `Test` permits everything. Going through
`FindGroup` would silently ignore a site that disallows crawling wholesale.

## Retries

Retried: `429`, `503`, `408`, and other `5xx`, up to three attempts, with
exponential backoff from one second and up to 25% jitter.

Not retried: any other `4xx`. A `404` will still be a `404` in ten seconds, and
retrying it is one more request the origin did not need to serve.

`Retry-After` is honored when present, in both the delta-seconds and HTTP-date
forms — **up to five seconds**. Past that the response is returned unretried and
the job's own backoff reschedules it.

That threshold is about where waiting happens. Sleeping inside a request holds
a worker slot and a global concurrency token for the whole duration, so obeying
a two-minute `Retry-After` inline would park scarce capacity on a host that has
just said it is busy. The job queue is the right place to wait minutes; the
HTTP client is not.

This was found by a test that took 259 seconds instead of one. The threshold
started at two minutes, and a fixture returning `Retry-After: 120` was faithfully
slept through, twice.

## Identification

The User-Agent is `tomekeeper/<version>` plus, when `TOME_CONTACT_URL` is set,
`(+<url>)`.

No browser spoofing, ever, and it is not configurable to a browser string. An
operator who wants this archiver to stop should be able to work out who to ask
in one look at their logs. Setting `TOME_CONTACT_URL` to a real page of your own
is strongly encouraged before pointing this at anyone else's server — it is the
difference between a bot someone can contact and a bot someone can only block.

## Limits that protect everyone

- 10 second connect timeout, 30 second total per attempt.
- 10MB response cap. Larger is a malfunction or an attack, and the cap reports
  an error rather than truncating — handing a half-downloaded document to a
  parser produces a confusing failure much further along.
- Two idle connections per host, deliberately low.

## The other direction: what the fetcher may reach

Everything above is about being welcome on servers this archive does not own. The
mirror image is that a request made on somebody else's instruction must not become a
way to read what only this machine can reach.

That is not hypothetical. On 2026-08-23 a subscribed feed redirected a poll to
`http://127.0.0.1`, and the fetcher dialed it — five times over an hour and three
quarters, with the failure backoff working perfectly around a destination it should
never have tried. That one hit a closed port. On this deployment the worker can also
reach the Kubernetes API and the archive's own Postgres, and a body fetched from
either would have been stored as an article and rendered in the reading interface.

**Nothing outside the public internet is fetched**, unless an operator names it in
`TOME_FETCH_ALLOW_PRIVATE`: loopback, RFC1918 and IPv6 unique-local, link-local
(which is where the cloud metadata service lives), carrier-grade NAT, the
unspecified and broadcast addresses, NAT64, and the reserved ranges. Refusals are
returned immediately rather than retried, because the address will still be internal
in twenty minutes.

Two layers enforce it, and they see different things:

- The **redirect check** sees the hop, so the error can say a redirect caused it.
  That is what makes a misconfigured feed diagnosable from the attention queue
  instead of reading as "connection refused" against an address nobody typed. It
  cannot be the guard: it sees a host name it has not resolved.
- The **dial hook** sees every address the resolver returned, whoever asked and
  however they got there — a redirect, a reader pasting an address into **Save a
  page**, or a perfectly public host name that resolves to `10.43.0.1`. That last
  case is DNS rebinding, and no amount of URL checking finds it, because the URL is
  honest.

The escape hatch is deliberately narrow. Somebody archiving their own wiki names
that network, or that host, and everything else stays refused — the alternative, a
single switch, means opening the metadata address to open a NAS.

**What this does not cover: the headless browser.** A page that needs JavaScript is
fetched by Chrome, in its own Deployment, over CDP — not through this client, and so
not through this guard. Restricting where that can reach is a NetworkPolicy on the
render Deployment, which is a deployment concern rather than something this code can
express. It is written here rather than assumed because the omission is exactly the
kind that reads as covered.

## What this does not do

Datacenter addresses get blocked by some content delivery networks regardless
of behavior, because a rate-limited archiver and a scraper look identical from
the outside. No code decision fixes this.

The response is an honest User-Agent, a respected `robots.txt`, an accepted
list of failures, and blocked domains surfaced clearly. Circumvention is a
non-goal, deliberately and permanently, and the failed-fetch queue exists partly
so that "this domain does not work" is visible information rather than a
mystery.

## See also

- [Configuration](../reference/configuration.md) — the rate and concurrency settings
- [Troubleshoot a failing feed](../how-to/troubleshoot-a-failing-feed.md)
- [Extraction and versioning](extraction-and-versioning.md)
