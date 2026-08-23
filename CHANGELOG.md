# Changelog

Notable changes, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Every release is a git tag `vX.Y.Z`, and the container image published for it
carries **the same string**: `ghcr.io/runlevel-six/tomekeeper:v0.17.0` is the tag
`v0.17.0`, and `tome version` inside it says `v0.17.0`. One identifier, everywhere,
so "what is running" has a single answer. See
[Cut a release](docs/how-to/cut-a-release.md).

## What the numbers promise

While the major version is `0`, the public interface is the HTTP routes, the CLI,
the environment variables, and the archive on disk — not the Go packages, which are
internal.

| Bump | Means | Upgrading |
|---|---|---|
| **Patch** (`0.1.0` → `0.1.1`) | Fixes only. **Never a database migration.** | Change the tag and apply. Nothing else to do. |
| **Minor** (`0.1.0` → `0.2.0`) | Features, and any release that adds a migration. May change defaults or remove a flag, with the removal noted here. | Run the migration Job, then apply. |
| **Major** | Reserved for 1.0. The Fever API landed in 0.2.0, so what is left is **multi-user** — after which this table stops having a caveat. | — |

"A patch release never migrates" is the load-bearing half, and it is enforced by
`scripts/check-release.sh` rather than remembered: it means a patch upgrade cannot
require anything but changing the tag.

An **extraction version bump** is called out under its own heading whenever it
happens, because it is the one change that wants a follow-up command
(`tome reextract`) to reach articles already in the archive.

## [Unreleased]

Nothing yet.

## [v0.17.0] — 2026-08-23

### Added

- **Search finds titles**, not only bodies. An article whose distinctive words appear
  only in its title was unfindable — discovered while running the multi-user
  acceptance drill, where searching "Desktop" for an article titled *An Atari Desktop
  On A Sega* returned nothing, which looked for a moment like a scoping failure. A
  title is the string a reader actually saw in a list, and a body legitimately need
  not repeat it.

  **An article that failed extraction is now findable too**, by the title it does
  have. The body join was an inner join, so the pages this archive could not read were
  also the pages it could not find — and those are exactly the ones somebody goes
  looking for by name.

  A title match ranks above every body-only match. Body ranks are normalized into
  `[0,1)` and a title match adds 1, which is a statement rather than a weighting: if
  the words are in the title, that is the article you meant. Found while writing the
  test — bare `ts_rank_cd` is unbounded, so a body repeating a word forty times had
  been beating the article named after it.

  Migration `00022` adds `articles.title_tsv` and its index. Titles are short, so it
  is a fraction of the size of the index over the bodies.

### Fixed

- **Four pages were unstyled, Accounts most visibly.** `class="page"` was written into
  the markup of Accounts, the extraction explanation, the audit, the reprocess page and
  the set-a-password page — and there was no rule behind it, so they rendered
  full-width with body-font headings and no separation between sections, while Settings
  looked like the rest of the application because it spelled the same intentions out
  under its own class. The pattern now exists once and those pages share it, along with
  the account list's table, the setup link, and the row controls.

  The extraction ladder was worse: its table had no rule at all, and `.won` — the
  class marking the rung that produced the stored body — drew nothing. The winning
  rung is now marked.

  **A test now fails when a template uses a class the stylesheet does not define**,
  with an allowlist naming the classes that are deliberately unstyled and why. Section
  names are hooks that take their appearance from the page; the point of the list is
  that the question gets answered in writing rather than discovered by somebody
  looking at a page that seems unfinished.


## [v0.16.0] — 2026-08-23

### Added

- **A body belongs to a reader.** The first half of tenancy: `article_content` and
  `domain_rules` gained an owner, so two readers can extract one shared page
  differently. Copy-on-write — `NULL` is the household's extraction, which is what
  everybody reads until their own diverges, so a household where nobody writes a
  domain rule stores exactly one body per article as before.

  **The expensive half stays shared.** One poll, one raw page, and one copy of each
  image however many readers hold it — images are 63% of this archive and are
  content-addressed, so they dedupe regardless of who extracted what. Every current
  body is about 10% of the bytes, which is what a reader forking the entire archive
  would cost.

  This is the schema and the read path: the stream, the article, search, the Fever
  API, the export, the saved list and the body chooser all now show a reader their
  own body, falling back to the household's. What *creates* a reader's body is the
  next piece of work; today only promoting does.

  **A domain rule's two halves have different owners.** The content and strip
  selectors decide how a stored page becomes a body, so a reader may hold their own
  and get their own extraction. Whether a page needs a browser, what user agent
  fetches it, and how fast — those decide how it is *retrieved*, and it is retrieved
  once, so they stay the household's. A reader can decide what their copy of a page
  says; not that the archive fetches it twice.

  A reader's rule wins over the household's for that reader **even when the
  household's names a more specific domain**, because specificity orders your own
  rules against each other rather than against somebody else's.

  **Writing a rule now extracts the articles it covers**, and says how many. On the
  domain-rules page an administrator chooses whether a rule is theirs or everybody's;
  a reader always writes their own. The page lists your rules and the household's and
  never another reader's — showing you that somebody has a rule for a host would tell
  you they read it.

  **A sweep re-derives that work every minute as a backstop.** The eager enqueue
  happens in the request that saved the rule and does not happen at all if the worker
  is down — which, with the server and worker as separate Deployments, is every
  rollout, every migration wait and every OOM. Without the sweep a rule saved in one
  of those windows would appear to have been accepted and never be applied to a single
  article. Every other stage of the pipeline already pairs eager enqueueing with a
  sweep for the same reason.

  **`tome reextract --user NAME`** brings one reader's own bodies forward instead of
  the household's. It reaches only bodies they already have — a reader without one
  reads the household's, and giving them a private copy of every article in the
  archive is the opposite of what copy-on-write is for. Applying a reader's rules to
  a host they have no bodies from is a different question, and **Reprocess** on their
  rule's row asks it.

  A reader's extraction writes only their body: not the article's title, not its
  attempt version, not its images' status, not the failure recorded against it, and
  not `index.html` on disk. One reader's selector must not rename an article in
  everybody's list, and their success does not mean the archive managed the page.

- **Accounts have a role, and sessions can be revoked.** The first half of
  multi-user, and on its own it closes a hole that was live: `requireUser` trusted the
  user id sealed in the session cookie without checking that the account still existed,
  so deleting a reader left them signed in until their cookie expired. Nothing leaked —
  every query is scoped to a user id with no rows — but the archive could not actually
  turn anybody out.

  `users.session_epoch` is sealed into the cookie when it is issued and compared on
  every request, so bumping it signs that reader out everywhere at once. That is what
  deleting an account, changing a password, and an explicit sign-out-everywhere all
  need. It buys revocation per reader rather than per device, which is the trade taken
  instead of a sessions table — and the session interface still allows one later.

  `users.role` is `admin` or `reader`, and admin is about changing what everyone
  shares rather than about what anybody may read. The account `tome migrate` seeds is
  an admin, because it is the operator. **An admin-only page answers 404 rather than
  403** to a reader, on the same reasoning that makes another reader's article
  not-found: a 403 confirms the route is there.

  **Everybody is signed out once on upgrade.** The session payload gained a field, so
  credentials issued before it no longer parse. Accepting the old shape would have
  meant assuming an epoch for exactly the cookies the epoch exists to revoke.

- **Accounts, for more than one reader.** An administrator can create accounts, issue
  a link that sets a password, and delete an account — from **Accounts** in the
  interface, or with `tome user` when nobody can sign in to reach it.

  A new account has **no password and cannot be signed in to**, and there is no
  sign-up page. The way it gets one is a **single-use link**: whoever opens it chooses
  the password, so nobody else ever learns it. The link works once, expires after a
  week, and issuing another supersedes it — only a hash is stored, so it is shown once
  and cannot be looked up again. The same link is how a forgotten password is reset.

  **Deleting a reader keeps the archive.** Their subscriptions, tags, highlights and
  reading state go; every article and image stays, because nothing an article is made
  of belongs to a reader. What is left is articles nothing references, which is what
  `tome prune` reports. **The last administrator cannot be deleted or demoted** — an
  archive without one cannot make another through the interface.

- **Forgetting**, under Settings. How long your reading stays yours: after your
  window, articles you have read drop off your lists and the record of having read
  them goes. Anything starred, saved, kept or **highlighted** is never forgotten —
  annotations are the one thing a reader may value more than the article, and deleting
  them on a timer is not a trade anybody asked for.

  **The archive keeps the articles until everybody has forgotten them**, so your
  choice never costs anybody else anything. Forgetting is what releases your claim,
  and the stored copy goes only when no claim is left.

  **`TOME_RETAIN_AFTER_READ` is now the default window rather than a global
  deadline**, and that is a correctness fix as much as a feature: expiry used to
  compare every reader's read time against one archive-wide cutoff, so once windows
  could differ, somebody asking to keep things for a year would have had their claim
  released after the archive's thirty days and lost articles they had said they
  wanted.

  Engaging with an article un-forgets it. Reading, starring, keeping or saving one
  restores the claim — otherwise the archive would be saying "nobody wants this" about
  something you are looking at.

- **Why this article looks like this**, at `/articles/{id}/explain` and from **Why?**
  on any attention-queue row. It runs the extraction over the page already on disk and
  reports which rules applied — yours or the household's — what each step produced,
  and whether what is stored is stale.

  This existed as `tome explain`, which needed a terminal and, on Kubernetes,
  permission to exec into a pod. Once a reader can write their own rules, the person
  who most needs to know why a selector produced nothing is the one least likely to
  have either. The command and the page now call the same function, because two
  implementations of "what would the ladder do" would drift, and an explanation that
  no longer describes the extraction is worse than none. `tome explain --user NAME`
  explains what one reader sees.

- **Change your own name**, under Settings. It asks for your password, and not only
  because a rename changes how you sign in: the Fever API key is derived from your
  name *and* your password and is computed by the client, so renaming without
  rewriting it would leave every mobile client authenticating against a key nobody
  can compute — silently, since the client has no way to be told. It cannot be
  recomputed from the stored hash, which is why the form asks.

- **Delete your own account**, under Settings. Leaving no longer requires asking an
  administrator. Your subscriptions, tags, highlights and reading state go; every
  article and image stays, so nobody else loses anything. It asks first, and asks for
  your password. The last administrator is refused — an archive without one cannot
  make another.

- **Change your own password**, under Settings. It asks for the current one, then
  signs out your *other* browsers and keeps the one you are using: a password change
  that threw you out of your own session would be a poor way to secure an account.

- **Reprocess your archive**, at `/reprocess`, from Settings and from the foot of the
  rules page. Extraction runs again over the pages already stored, with your rules as
  they now stand — nothing is fetched, so it costs nobody's server anything and works
  for sites that no longer exist.

  It counts before it acts, and counts two things separately because they answer
  different questions: the bodies from an older extractor version, which is the
  follow-up to an upgrade, and *all* of yours whatever version they came from, which is
  what a spell of editing rules wants — a rule is data rather than a version, so a body
  it would now change is not out of date by any number.

  **Whose bodies move is the other axis, and the page keeps them apart.** Yours affect
  nobody else. The household's are what every reader sees unless they hold their own, so
  that half is offered to administrators only and **refused** to anybody else rather
  than quietly downgraded to their own bodies. Holding none of your own is the ordinary
  state and the page says so rather than offering a button over a count of zero: copy-on-write
  gives you a body only where your rules produce something different.

- **Bodies worth a look**, at `/attention/audit`, linked from the foot of the attention
  queue and from Settings. The attention queue lists what did not arrive; this asks the
  harder question underneath it — what arrived and is *wrong* — which is the failure
  that removes itself from that queue by succeeding. Three lenses, scoped to the
  articles you can see and to the body each of them shows *you*.

  This existed as `tome audit`, which needed a terminal, and its queries are
  archive-wide — right for an operator maintaining an archive and wrong for a page. The
  command keeps that view; the page answers the reader's question.

  **A page of its own rather than a section of the queue, and that was measured**: the
  three lenses cost about 1.1 seconds over 2,264 bodies, almost all of it the title
  lens. Inlining them would have charged that to every visit to the attention queue,
  including the visits where they find nothing and nothing is shown. So the link is
  always there and the work happens when you ask for it.

  Nothing here is a gate and some of it is meant to be a false alarm — measured over a
  real archive, the title lens flagged seven bodies and not one was a body extraction
  had got wrong. The page says so above the findings, because a list of complaints with
  no stated precision reads as a list of faults.

- **Sign out everywhere**, under Settings. Ends every session signed in as you,
  including the one you are using, for the case where you signed in on a machine you
  no longer control. It asks first, like the bulk mark and unsubscribe do, because the
  content security policy leaves no room for a JavaScript dialog and most of the effect
  is on devices you are not looking at. Mobile clients are unaffected — they hold a key
  derived from your password, not a session, and it says so on the page.

### Removed

- **`tome reextract --since-version`**, the deprecated alias for
  `--target-version`. The name read as an ordering — "everything from version 2
  onwards" — and the selection is "any version other than this", so passing the
  version your bodies were already at selected nothing and reported success. That
  cost an hour once. It goes before 1.0 freezes the CLI rather than being kept
  permanently: a name whose natural reading is a trap is a worse promise to keep than
  a written-down command is to break.

- **`article_content.fs_path`** (migration `00021`), a column nothing ever wrote and
  nothing read. It was meant to record where a body's standalone page went in the
  archive tree; on the live archive it was `NULL` on all 10,161 rows, because the path
  is derived from the article identically at each call site. Dropped rather than
  populated for what 1.0 means: the schema is part of the interface, and a documented
  column that always lies by omission is the same shape as `assets_status = 'pending'`
  being a terminal state wearing a transient label. `assets.fs_path` is a different
  column on a different table and stays.

### Changed

- **Promoting a stored copy is now your choice alone.** It copies the body you picked
  into your own slot instead of changing the shared one, so it decides what you read
  and nothing about what anybody else does.

  Two concrete reasons rather than a preference: an imported body has an importer, so
  one reader's library should not become another reader's article text — possibly of
  a paywalled page they never had access to; and highlights anchor by quoted text
  rather than by body id, so changing a body under somebody could silently strand
  their annotations.

  A promoted copy **stops receiving extraction improvements** until reader-scoped
  reprocessing exists: `tome reextract` brings the household's extraction forward and
  a promoted copy has left that lineage. That is right — one reader's choice should
  not make work for everybody — and it is the one thing the change costs.

- **Changing a password signs out existing browser sessions.** It always disconnected
  Fever clients, because that key is derived from the cleartext; leaving browsers signed
  in meant a password change was no change at all to whoever already had a session.

  **`tome migrate` no longer rewrites a password that has not changed**, which is what
  keeps this from signing you out on every deploy: `TOME_PASSWORD` is a Secret key, so
  the migration Job has it in hand every time it runs, and it used to store it
  unconditionally. It now verifies against the stored hash first and says "nothing
  changed" — and the "mobile clients will need reconnecting" line is finally printed
  only when that is true. Comparing hashes would not work: argon2id salts randomly, so
  the same password never hashes to the same string twice.

- **Every page now shows the signed-in reader's own name.** It read `TOME_USERNAME`,
  which names the account `tome migrate` seeds and nothing else — invisible while
  there was one account and wrong the moment there were two.

- **The sign-in page no longer prefills a username.** It was a kindness on a
  single-user first run and is a disclosure once there is more than one account: it
  names a reader to anyone who loads the page. The "no password is set" hint stayed by
  asking a better question — whether *any* account in the archive can be signed in to,
  rather than whether a named one can — which is the condition it was always about.

### Fixed

- **The fetcher followed redirects into this machine's own network, and no longer
  reaches it at all.** A subscribed feed redirected a poll to `http://127.0.0.1` and
  the fetcher dialed it — found live on 2026-08-23, five attempts over an hour and
  three quarters, with the failure backoff working perfectly around a destination it
  should never have tried. That one hit a closed port. The same path could have
  reached this deployment's Kubernetes API or its Postgres, and a body fetched from
  either would have been stored as an article and rendered in the reading interface.

  **Nothing outside the public internet is fetched now**: loopback, RFC1918 and IPv6
  unique-local, link-local — which is where the `169.254.169.254` metadata address
  lives — carrier-grade NAT, NAT64, the unspecified and broadcast addresses, and the
  reserved ranges. A refusal is not retried, because the address will still be
  internal in twenty minutes.

  Two layers, because they see different things. The redirect check names the hop, so
  a misconfigured feed is diagnosable from the attention queue instead of reading as
  "connection refused" against an address nobody typed. The dial hook judges every
  address the resolver returned, whoever asked and however they got there — which is
  what also closes a reader pasting an internal address into **Save a page** or
  **Add a feed**, and DNS rebinding, where the URL is honest and the answer is not.

  `TOME_FETCH_ALLOW_PRIVATE` opens named networks, addresses or host names, for
  somebody archiving something on their own network, and is empty by default. A
  misspelled entry fails at startup rather than silently matching nothing.

  **The headless browser is not covered**, and that is stated rather than assumed: a
  rendered page is fetched by Chrome in its own Deployment, so restricting where
  *that* can reach is a NetworkPolicy rather than anything this code can express.

- **`tome domain-rule set example.com --selector .post` now saves the rule** instead
  of printing usage. Go's flag parsing stops at the first non-flag word, so anything
  after the domain was a stray argument — and the same trap silently dropped
  `--base-url` from `tome user link jane --base-url …`. Both take their argument from
  wherever it appears now. It was documented as "flags first" for two releases, which
  is a way of asking every reader to remember something the program can simply
  accept.


- **A fetch that ran out of time could not record that it had.** Every job gets a
  context with a one-minute deadline, and the failure was written through that same
  context — so the one outcome that could never be stored was the one where the time
  ran out. The article stayed `pending` with no reason against it, which is a state the
  attention queue does not list, and the fetch scheduler enqueues every `pending`
  article it finds. Found on this archive as one article whose host had stopped
  answering: four days, seventeen attempts, each one failing at the write rather than
  at the fetch, and nothing anywhere saying so.

  Outcomes are now written on a context detached from the job's own deadline. A page
  that does not arrive in the time allowed lands in the attention queue like any other
  failed fetch, with "the fetch ran out of time" against it rather than the name of a
  Go value, and `tome refetch` is the way back.

  **A worker shutting down mid-fetch is still not the page's failure.** That case is
  told apart from running out of time and left alone, because River hands an
  interrupted job to the next worker that starts — recording it would have permanently
  failed whatever was in flight during every rolling restart.

- **The audit lenses judged an article rather than a body**, which was right until an
  article could have two current bodies and wrong from the moment one could. With a
  reader's fork beside the household's extraction, the title lens counted the title's
  words twice, treated "some body mentions the title" as good enough — so one sound body
  hid a broken one — and reported the same finding once per body. Each lens now judges
  one body, and "a body more than one article shares" counts distinct *articles*, so a
  fork that happens to match the household's byte for byte is no longer reported as an
  article sharing a body with itself.

- **Fetching a page again now comes back to the page you were on.** The button posted
  where it came from and nothing read it, so every one of them landed on the attention
  queue — which threw a reader off the audit page after the first of four fixes. The
  destination is matched against a fixed set rather than followed, because a redirect
  built from what a form posted is an open redirect.

- **A precision figure in v0.15.0's notes was wrong.** They said the title lens flags
  seven bodies on this archive and "about three are real". That was inferred from word
  counts and domains rather than read; reading all seven refuted it — not one is a body
  extraction got wrong. Two are artifacts of the URL-title bug fixed in the same release,
  and the rest are two podcast episode pages, a link roundup, a digest in Russian, and a
  store homepage with no article in it. The corrected numbers argue the same conclusion
  harder: as a rejection rung the lens would have discarded a 16,249-word body and five
  legitimate ones to catch nothing.

## [v0.15.0] — 2026-08-22

### Added

- **`tome audit`** reports stored bodies that may not be what they claim to be. The
  failed-fetch queue answers *what did not arrive*; this answers *what arrived and is
  wrong*, which nothing asked until a re-fetch stored a site's cookie consent dialog as
  a 410-word article and, by succeeding, removed it from the queue.

  Three lenses: bodies sharing no distinctive word with their title, bodies that more
  than one article shares, and titles that are URLs.

  **None of them is a gate, deliberately.** Measured over this archive's 2,211 bodies,
  the title lens flags seven and about three are real — as a rejection rung it would
  have thrown away a 16,249-word body because that article's title was percent-encoded.
  A check with a third of the precision it needs is worse than no check when its action
  is to discard an article. So it prints and changes nothing.

  It looks only at the `trafilatura` and `readability` rungs, which choose a block of a
  page and can therefore choose the wrong one. A `domain_rule` body was 217 for 217
  here — a hand-written selector cannot wander — and a `page_images` body is a picture
  with no prose to match, so flagging comics would bury the list.

### Fixed

- **Titles that are URLs are no longer permanent.** `UpdateArticleMetadata` fills gaps
  only, which is right — a feed's title is a choice somebody made — but a URL is not a
  choice anybody made, and treating it as one left twelve articles titled with their own
  address, one of them 16,249 words under `eBPF%20and%20the%20Cilium%20Datapath.pdf`. A
  placeholder title now counts as a gap, so the page gets to replace it and a
  `tome reextract` repairs the existing ones with no new command. The importer also
  decodes an escaped filename rather than storing the escapes.

- **CI failed the DCO check on a commit that was correctly signed.** The check compares
  against `github.event.before`, which a force-push leaves naming a commit that was
  rewritten away — absent from a fresh clone, so the range would not resolve and the
  step failed. Amending a signed commit is not a missing sign-off, and the message
  pointed at the sign-off rather than at the range, sending the maintainer looking for a
  mistake he had not made. An unresolvable base now falls back to the pushed tip, the
  same way a branch's first push already did, and says which commit it settled for.

- `tome refetch` reported "Queued 8 fetchs". `plural` appended a bare `-s`, which is
  wrong after a sibilant, so the helper now takes `-es` after ch, sh, s, x and z.

### Changed

- **stackoverflow.blog is no longer marked as needing a headless render.** The flag was
  true of the posts that motivated it — a direct fetch of them measures 2,432 and 2,745
  visible characters against posts thousands of words long — but the browser cannot
  reach them either: rendering lands on the site's cookie consent gate, so the captured
  DOM carries the consent dialog and readability lifts *that* as the body. The result
  was a confident 410-word article that was a cookie notice, which also took the
  article out of the failed-fetch queue. A visibly bodyless article is the better
  failure. The rule's notes carry the measurements and what would let it be turned back
  on: a renderer that dismisses the consent gate, or waits for the article to hydrate.

## [v0.14.0] — 2026-08-21

### Fixed

- A test proved the wrong thing intermittently, failing about half of CI's runs on
  master. `TestAPageIsFetchedAgainOnlyWhenAsked` asserted that an ordinary enqueue
  does not re-fetch a page the archive already has — through the job queue, where
  fetches are unique per article across every non-terminal state. Proving the worker
  declined therefore required the previous fetch to be finished first, and a stored
  body does not show that: extraction is enqueued by the last statement of the fetch's
  own work, before it returns, so the extract job can run to completion while the
  fetch job is still `running`. The insert was refused as a duplicate and the test
  said so rather than passing on it. The refusal is now proved by calling the worker
  directly, which needs no timing at all; the queue keeps the half it is good for.

### Added

- **`tome refetch <id>...`** does from a command what the failed-fetch queue's button
  does per row. A repair is rarely one article: a site whose image URLs expired takes
  every article from that site with it, and a domain flagged for a browser after the
  fact takes everything fetched before the flag. Reports by default and queues only
  with `--yes`, because every article costs one request to somebody else's server.

- **`tome prune`** collects the residue unsubscribing leaves: articles no feed
  references and nobody has acted on. Retention cannot reach them — it only ever
  expires articles that were *read*, so one that arrived, was never opened, and then
  lost its feed is never expirable at any setting — and unsubscribing deliberately
  deletes no articles, because re-subscribing relinks them by canonical URL. So
  nothing had ever collected them.

  It **releases bodies**, exactly as retention does, rather than deleting article
  rows: the archive keeps knowing the article existed. Anything read, starred, saved,
  or imported is never a candidate, and an imported body is refused for the reason
  retention refuses it — it may be the only surviving copy of a page that is gone.

  **Reports by default and acts only with `--yes`** — the opposite convention to
  `reextract --dry-run`, deliberately: re-extracting is free and reversible, while
  this releases bytes that would have to be fetched again. It reports the bytes rather
  than only the count, because "prune 812 articles" is not a decision anybody can take
  and "recover 140 MB" is.


## [v0.13.0] — 2026-08-21

**Adds a migration.**

### Added

- **Fetch a page again**, from the row in the failed-fetch queue where the problem is
  noticed. Extraction runs over stored bytes, so when the bytes themselves are wrong
  no amount of re-extracting helps and only the origin can — two live cases: a site
  whose images sit behind URLs that expired before a rule existed, and a page that
  needed a browser before anybody flagged the domain. A flagged domain is handed to
  the browser on the way through, which is what makes it the fix for the second.

  Never automatic. The fetch worker still refuses a page it already has unless asked,
  because a re-fetch is a request the origin did not need to serve, and nothing in
  the pipeline asks. A POST rather than a link, so no crawler or prefetcher can spend
  the request for you.

  The page is **overwritten in place**, at whatever path the article already points
  at. Recomputing it would usually agree, but a directory is named after the article's
  title and extraction fills that in after the first fetch — so a re-fetch would put
  the new page in one directory and leave the `index.html` and localized images in
  another.

### Migrations

- **00014** drops `feeds.category`, which `00013` superseded and deliberately left
  behind. It was kept because the schema guard treats a newer database as safe on the
  grounds that an older binary works against a superset schema — true only while
  migrations are additive. It is safe to drop now that no deployable binary reads it.

## [v0.12.1] — 2026-08-21

### Fixed

- **The unread count no longer drifts while you read.** It is shown in four places —
  the document title, the nav badge, the tab bar badge and the installed app's icon —
  and every one of them was rendered at page load and then left alone, so marking
  articles read by scrolling silently made all four wrong until the next navigation.
  The response now carries the fresh total and the page applies it everywhere.

  Fixing only the app icon was the obvious small change and would have been worse:
  two numbers on one screen that disagree read as a broken count, where four equally
  stale ones read as a page that needs reloading.

  The count also re-asserts itself on the app icon when the app returns to the
  foreground, since the platform may have cleared it while away.

## [v0.12.0] — 2026-08-21

**Adds a migration.**

### Added

- **Categories can be created, renamed and deleted from the interface.** A category
  used to be free text on a subscription, existing exactly as long as some feed
  claimed it — so there was no object to create, an empty folder was impossible, and
  renaming one meant rewriting every feed in it.

  **Deleting asks what happens to the feeds** — leave them filed under nothing, move
  them to another category, or unsubscribe them — and **no answer touches an
  article.** Nothing in this project deletes one, and an article has no category of
  its own to lose: it is derived through `feed_items` to the feed that carried it, so
  refiling a feed moves everything it ever brought in. The unsubscribe option says
  plainly that the archive is kept *and* that anything never opened stops being
  listed, which is the same consequence unsubscribing one feed has always had.

  The nameless bucket is deliberately not manageable. It is the absence of a category
  rather than one named for absence, so there is nothing there to rename or delete.

- **The feed form offers the categories that exist, and an explicit "no category".**
  It was a free-text field with suggestions, on the grounds that a category existed
  only because some feed claimed one — there is a list now. The old arrangement also
  had a real gap: filing a feed under nothing meant *emptying* the field, which
  worked and which nobody discovered. A companion field still names a new category,
  and a typed name wins over the picker, which the form says out loud.

### Fixed

- **Renaming a category no longer reshuffles a synced client's folders.** The Fever
  group id was a hash of the category's *name*, because the protocol requires an id
  and there was no row to take one from — so a rename made the old folder vanish from
  a client and a new one appear holding the same feeds. Clients cache folder
  membership against those ids. The id now belongs to the category, and 57 lines of
  hashing and collision-rehandling are gone.

### Migrations

- **00013** adds `categories` and `feeds.category_id`, backfilling from the old
  column. `feeds.category` is deliberately **not** dropped: `internal/db`'s schema
  guard treats a database newer than the binary as safe on the stated grounds that
  "the old binary's queries still work against a superset schema", which is true only
  while migrations are additive. Dropping it would leave an older binary passing the
  guard and then failing on every query naming it — the outage that guard exists to
  prevent, from the other direction. It is frozen at the backfill and droppable once
  no deployable binary reads it.

## [v0.11.0] — 2026-08-21

### Added

- **Pull down from the top to reload**, on any page. It follows the reload control,
  which is the same thing `r` does — installed on a phone there is nothing else that
  can ask for newer articles: no address bar, no reload button, and no
  pull-to-refresh of the platform's own in a standalone window. The comment on that
  control has said so since it was written.

  It reloads the page and does **not** poll the feeds. That control is on the Feeds
  page and labeled, because it costs every subscribed site a request, and a gesture
  this easy to perform must not be the one that spends them.

  Where the browser has its own pull-to-refresh — Android's does — it is now
  suppressed, so the two cannot both fire and reload twice. In a standalone window
  there was never one to suppress, which is the case this exists for.

  The header follows the finger and the reload glyph turns as the pull completes;
  past the threshold the control highlights, the same way the mark-read control does
  at the end of a list. Which gesture fires is decided by direction while the drag is
  happening, so a long diagonal cannot satisfy both the swipe and the refresh and
  navigate twice.

## [v0.10.1] — 2026-08-21

### Fixed

- **A text size no longer changes the layout width.** Every step now gets the column
  the largest step produced, which is what it was asked for after use. The widths are
  pinned to that step's value and divided back out by whatever scale is in force, so a
  column follows the reader's *browser* font size but not their chosen text size —
  changing the text size changes the text and nothing else.

  This reverses the reasoning the preference shipped with, which grew the column with
  the type to hold roughly 68 characters per line. It no longer holds: the largest step
  gives nearer 71 characters and the smallest nearer 97. A stable layout was preferred
  to a stable measure, deliberately, and it is written down so nobody restores the old
  behavior thinking it was an oversight.

  The sign-in box still hugs its content — nobody is signed in there, so there is no
  chosen size to honor. Breakpoints needed no change: `rem` inside a media query
  resolves against the browser's initial font size rather than the root element's, so a
  text size cannot move one.

## [v0.10.0] — 2026-08-21

**Adds a migration.**

### Added

- **A text size preference in Settings**, four named steps. Like the palette it is a
  column on `users` rendered into the first paint rather than a cookie read by
  script — and for a sharper reason: a size applied after layout reflows the whole
  page under somebody who has started reading it.

  It scales the **root** font size as a percentage, so one number moves every view
  and it multiplies whatever font size the browser is already set to instead of
  replacing it. **Layout widths stay put** — they are pinned to the widest step's
  value, so changing the text size changes the text and nothing else. Archived
  standalone pages keep their own typography, as they must: they have to open with
  none of this running.

### Fixed

- **The interface overrode the browser's own font-size setting.** `body` was pinned
  at `16px`, so everything inheriting from it ignored a reader who had already asked
  their browser for larger text. It is `1rem` now. This changes rendering for anyone
  whose browser font size was not the default — in their favour.
- **The article had four supporting type sizes pretending to be distinct.** The
  byline at 12.8px, the outbound link at 13.6, the image notice at 13.6 and captions
  at 14.2 — 1.4px apart on a phone, which reads as one size with rounding errors —
  and none of them moved when the body did. Now one supporting tier derived from the
  body, so the article holds its proportions at every width and every size step.
  Three levels rather than four is deliberate: four do not fit in the range a phone
  has available.

## [v0.9.0] — 2026-08-21

### Fixed

- **The README generated an unusable Postgres password about 40% of the time.** It
  still said `openssl rand -base64 24`, which was corrected in
  [Install on Kubernetes](docs/how-to/install-kubernetes.md) and nowhere else:
  base64 contains `/`, and a `/` ends the authority section of
  `postgres://tome:PASSWORD@host/db`. 32 base64 characters carry one with
  probability 1 − (63/64)³². The other two secrets on that line are not put in a
  URL and stay base64. `scripts/check-release.sh` reads `deploy/` and
  `compose.yaml` only, so nothing was ever going to catch this but reading it.

### Added

- **Swipe left-to-right on an article to go back to the list.** Follows the same
  link `u` does, so there is one way back rather than a second implementation of
  one. Left-to-right because that is the direction the whole device uses for this;
  right-to-left deliberately does nothing, since previous and next are buttons and
  the article nav sits at both ends of every article.

  It gives up if the drag turns into a scroll — judged on the way, not at the end,
  because reading is mostly vertical scrolling — and a drag that begins inside
  something scrollable sideways is left alone, so the wide code blocks and tables in
  archived bodies stay usable. The article follows the finger while you drag, which
  is the only feedback available: the nav it would otherwise highlight is off screen
  from the middle of an article.

- **`task test:js`** — the touch gestures, run against a stub DOM by node and
  nothing else. No package.json, no framework, no build step. It earned its place
  immediately: two of the six guards it now covers turned out to be untested by the
  sequences written first, which the neuters found and hand testing could not.

## [v0.8.0] — 2026-08-21

### Fixed

- **`scripts/check-release.sh` could silently stop checking an overlay.** It read
  the version pin through a two-line window after the image name, so a comment
  written in between pushed the pin out of view — and an unparsed override was
  *skipped*, not failed. A half-finished version bump would have passed, with no
  trace but a count in a pass message dropping from 3 to 2. It now reads the whole
  image entry, and recognizes a `digest:` pin as the other legitimate way to name
  an image rather than as an absence.

### Added

- **A list can be marked read from its end, and on a touch screen by pulling past
  the bottom.** The control beside the heading is the wrong one to reach for after
  reading forty pages — it is forty pages up — and marking read as you scroll can
  never reach the last screenful, because those rows never leave over the top edge.
  So the bottom of a list was exactly where a reader had finished and had nothing to
  say it with.

  Both controls are the same request and the same two-step confirmation; there is no
  second path to the write. The pull gesture follows the link rather than posting,
  so an accidental pull costs a page you navigate away from. With no JavaScript the
  control is still there to tap.

  The end-of-list control is drawn by whichever render reaches the end — the
  document for a short list, the last appended fragment for a long one — because
  rows are appended as they are revealed and nothing fixed in the document is ever
  at the bottom. Its unread count is counted on that final page only, not once per
  page on the way down.

## [v0.7.0] — 2026-08-21

Two corrections to v0.6.0, both found by running it against a real archive rather
than by reading it. **Adds a migration.**

### Extraction version 7

**Version 6 reached one of the ten strips it was written for.** It matched a short
slug against an image's whole file name, which was generalized from a single
example: the site in question names its files `171-err.png`, and only one strip
happened to be `10x.png`. The signal it missed was the folder —
`/2020/err/171-err.png` — so a short slug is now matched against any complete
**path component**, which is what "strong enough a claim to trust" actually meant.

Measured over every article in a real archive whose slug is under four characters,
which is the only set this branch can affect: **7 gained their strip, nothing else
changed.**

Run `tome reextract` once after upgrading.

### Fixed

- **A reprocess that produces nothing no longer files an article that already has a
  body as a failure.** A version bump runs the current ladder over every stored
  page, and a body produced by older behavior may simply not extract again — the
  reader still has the article and nobody can act on it. The version 6 catch-up run
  put eight such articles into the attention queue in one pass, hours after
  `00010` cleared out the last set. Same emptiness as that fix, arriving from the
  other direction.
- `store.Article` now carries `extract_attempt_version`, which until now was
  written and read only by the query that selects articles to reprocess. A column
  nothing reads is a shape of bug this archive has already found twice.

### Migrations

- **00011** repeats `00010`'s correction for the rows the version 6 run left
  behind. Same conditions, same reasoning: a stored page proves the fetch worked,
  and an imported body over a genuinely failed fetch keeps its failure.

## [v0.6.0] — 2026-08-21

Draining the extraction tail: seven domain rules, one rung that could not see short
slugs, and a queue that would not empty when you fixed it. **Adds a migration**, so
upgrading is the migration Job and then the apply.

The rules themselves are data rather than code — they live in the `domain_rules`
table and are set with `tome domain-rule set`, so nothing here ships them. What
shipped is the two things the rules could not fix by themselves.

### Extraction version 6

A slug too short to match as a substring is now matched against an image's whole
file name rather than discarded. The four-character floor was standing in for "is
this claim strong enough to trust", and an exact file name is a strong claim at any
length — so the rung written for image-only pages could not reach a strip at
`/2025/10x` named `10x.png`. Ten of them on a real archive, on a site where the
file name is always the slug.

Run `tome reextract` once after upgrading.

### Fixed

- **An article rescued from a page already on disk no longer stays in the attention
  queue forever.** An extraction that produces nothing is recorded as a *fetch*
  failure, and nothing ever took that back once a domain rule found the body. On a
  real archive, 409 articles with a good current body were still listed as failed,
  314 of them extracted by a rule. A queue that does not empty when you fix things
  is a queue people stop reading. The failure is now retired whenever an extraction
  becomes an article's current body and a stored page proves the fetch itself
  worked; an imported body whose page fetch genuinely failed keeps its failure,
  because the archive really is missing that page. Migration `00010` does the same
  correction once for everything extracted before this.
- **`tome explain` no longer reports a rule that matched something too short as a
  rule that matched nothing.** Those want opposite remedies — the page's markup
  versus the length floor — and the explanation sent every rejection to the first.

## [v0.5.0] — 2026-08-21

One fix, and it is the one worth upgrading for: extraction improvements can finally reach
the articles they were written for. **Adds a migration**, so upgrading is the migration Job
and then an apply — and then one `tome reextract` to collect the backlog.

### Fixed

- **`tome reextract` could not reach an article that produced no body**, which is the most
  expensive bug found in this project so far. Candidates were selected by comparing the
  extractor version that produced an article's *body* — so an article with no body was
  never a candidate, and **every extraction improvement since the second milestone
  silently skipped exactly the articles improvements are written for.**

  Measured when it was found: **343 articles with a stored page and no body, 280 of them
  webcomics from a single host** — and the image rung added three versions earlier
  archives them today. Their pages had been on disk since the first poll with nothing able
  to point at them.

  Failures now record the version that attempted them, so the same "other than this
  version" comparison works for both outcomes. **A one-off `tome reextract` after taking
  this release picks up the backlog**; it touches no origin server, as ever.

### Migrations

- **00009** adds `articles.extract_attempt_version`. Nullable, and compared with
  `IS DISTINCT FROM` rather than `<>` — `NULL <> '5'` is NULL, not true, so a plain
  inequality would have excluded every article the column was added to reach. That is the
  same shape of silent-exclusion bug it fixes, which is why it is called out here.

## [v0.4.0] — 2026-08-21

Headless rendering explains itself. Everything here came out of walking through what a
user actually sees when they add a site that turns out to need a browser — and finding
that the answer was mostly "nothing". **Adds a migration**, so upgrading is the migration
Job and then an apply.

### Added

- **The failed-fetch queue now says which remedy a page wants.** Each row reports how much
  visible text the *served* page carried: a few hundred characters is a JavaScript shell
  that wants a browser, thousands is a page whose structure defeated the extractors and
  wants a CSS selector. Those have opposite fixes and both used to read as "extraction
  produced no content", so telling them apart meant running `tome explain` against a pod.
- **"Waiting" is a state.** An article whose domain is flagged for rendering when no
  browser is reachable stays retryable *and* says so — in the queue, and as a `waiting`
  badge in the reading list. It previously sat pending forever, retried every minute,
  invisible to the queue, and badged `queued` with the tooltip "the worker has not reached
  this page yet". It had.
- The domain-rules page explains what flagging a host for JavaScript actually does, and
  what happens when no browser is deployed. It used to say rendering did not exist.

### Changed

- **The headless browser now runs by default** (one replica, ~256Mi idle) instead of
  scaled to zero. Deliberately spending memory on a feature most archives never use,
  because the alternative is a checkbox that silently does nothing: the reader who flags a
  domain and the administrator who can scale a Deployment are not the same person, and
  multi-user widens that gap. Scale it to zero to turn it off — flagged articles then wait
  visibly rather than failing.

### Migrations

- **00008** adds `articles.page_visible_chars`. Nullable, because NULL means "not measured
  since this existed" and a default of zero would have claimed every article in the archive
  served an empty page.

## [v0.3.0] — 2026-08-20

Sites that build their pages in JavaScript can be archived. **This release adds a
migration**, so upgrading is the migration Job and then an apply — not a tag change.

### Added

- **Headless rendering**, for the sites that send an empty shell and build the article
  in JavaScript. A stock `chromedp/headless-shell` Deployment ships **scaled to zero**;
  the worker renders a page only when a domain rule flags the host `requires_js` *and* a
  browser is reachable, so either alone does nothing. `requires_js` has been a column an
  operator could set and nothing could read since the schema was written; it now works.
  See [Enable headless rendering](docs/how-to/enable-headless-rendering.md).
  - **Rendering happens at fetch time, not as a rung of the extraction ladder.** What
    gets stored is the DOM the browser built, so extraction stays offline and
    `tome reextract` improves rendered articles without re-fetching anything — which a
    rendering rung would have made impossible.
  - Renders run on their own River queue at `TOME_RENDER_CONCURRENCY` (default 1), so a
    page whose script never finishes cannot consume the pool that polls feeds.
  - Images, media and fonts are refused **by resource type**, the archive's own
    User-Agent is sent, and robots.txt is checked before the browser starts. The
    unavoidable residue — the page's own JavaScript runs — is documented rather than
    glossed over.
- `TOME_RENDER_BROWSER_URL` and `TOME_RENDER_CONCURRENCY`.

### Changed

- `tome explain` reports whether a page came through a browser, and labels the feed
  body's size as markup — the ladder measures its *text*, and two unlabelled counts on
  adjacent lines read as a contradiction.

### Fixed

- **A rejected feed body reported "0 characters" whatever its length.** The rung zeroed
  its result before the explanation was built, so a body that missed the 200-character
  floor by one looked identical to one with no text at all. It now reports what it
  measured — 134 characters, on the article that exposed it. Extraction output is
  unchanged, so no `extract.Version` bump.
- `tome explain` announced any lookup failure as "no article N", including a schema
  older than the binary. A missing column now says so.

### Migrations

- **00007** adds `articles.browser_rendered`. Recorded at fetch time rather than
  inferred from the domain rules in force when somebody asks, because rules change.

## [v0.2.0] — 2026-08-20

Mobile clients can read the archive. No migration, so upgrading is a tag change and
an apply — but see the note under Changed about `TOME_SESSION_KEY`.

### Added

- **The Fever API**, so mobile RSS clients can read the archive. `POST /fever/`,
  authenticated with `api_key` — MD5 of `username:password`, the credential the
  protocol specifies and the reason `users.api_key` has existed since the schema was
  written. Groups, feeds, items, the two id-list sync calls, and marking an item, a
  feed or a group read. What a client gets in the `html` field is the **extracted
  body**, not the summary the feed shipped, which is the entire point.
  See [Fever API](docs/reference/fever-api.md) and
  [Connect a mobile client](docs/how-to/connect-a-mobile-client.md).
- **Signed asset URLs.** `GET /assets/…` now accepts either a session, as before, or a
  `sig=` this service issued. A Fever client renders a body in its own view with no
  cookie, and an `<img>` tag cannot carry a POSTed credential, so without this every
  picture in every client is a broken image icon. The signing key is derived from
  `TOME_SESSION_KEY` with its own HKDF label, so there is nothing new to configure —
  but rotating that secret now invalidates outstanding image URLs along with every
  session, and clients recover by re-fetching bodies. URLs last 30 days.

No migration. `users.api_key` was already there.

### Changed

- `TOME_SESSION_KEY` now derives two independent keys rather than one. Generating one
  at startup, which happens when the variable is unset, therefore also invalidates
  synced image URLs on every restart — the startup warning says so.

### Fixed

- **An import of a truncated export could run forever.** Running out of input was
  treated as a recoverable per-record problem, and since a decoder at the end of its
  input cannot advance, the adapter reported `record N: unexpected EOF` with `N`
  climbing until the process was killed. Both adapters, both the CLI and the upload.
  It was latent rather than new: the standard library reports a truncation as one of
  two unrelated error values depending on where the cut falls, and which one it uses
  moved in Go 1.27, so the shape the fixtures held was the one that stayed fatal.
  Running out of input is now fatal in either shape, which is what the two-pass import
  has always promised.
- **A truncated export now says the file is incomplete rather than naming a record.**
  A file cut between records has no bad record to point at, and the number it landed
  on was one past the end — "record 3" of a two-record file, which sends whoever is
  holding a half-downloaded library looking for something that is not there.

## [v0.1.0] — 2026-08-20

First tagged release. Everything below has been running against a real archive of
about 2,100 articles from 66 feeds (2,131 at the time of writing).

### Added

- **Feeds.** RSS, Atom and JSON Feed polling with conditional GET, per-feed
  adaptive intervals between 15 minutes and a day, exponential backoff, and
  auto-disable after 20 consecutive failures. OPML import from the CLI or by
  upload. Add a feed by pasting a site address — it follows the page to the feed
  it advertises and reports what it carries before you subscribe. Edit, unsubscribe,
  sort and filter the list; check every feed now rather than waiting.
- **Choosing how often feeds are checked.** A general cadence in Settings and an
  override on any one feed, with the feed's own setting winning. Neither can poll
  more often than `TOME_POLL_MIN_INTERVAL`.
- **Archiving.** Every article's page is fetched, the readable body extracted
  through a five-rung ladder, images localized and transcoded, and the result
  written to disk as a standalone page that opens in a browser with nothing
  running. Bodies are immutable and versioned; `tome reextract` re-runs extraction
  over stored pages without touching any site.
- **Reading.** Unread, everything, per-feed, per-category and per-tag lists with
  keyset pagination, full-text search across the archive, starring, saving,
  highlights, an attention queue for what did not come through cleanly, and
  mark-as-read on scroll. Six palettes plus a neutral default. Installs to a home
  screen and draws its own navigation there.
- **Typography.** Literata for prose and Inter for the interface, both shipped with
  the binary rather than named in a stack.
- **Politeness.** Per-host rate limiting, `robots.txt` honored for article and
  asset fetches (and deliberately not for feed polls), an honest `User-Agent`, and
  `Retry-After` obeyed inline up to five seconds.
- **Import and export.** A Wallabag library reads in; the whole archive writes out
  as one file the importer reads back.
- **Operations.** Kubernetes manifests including PostgreSQL, a migration Job, a
  nightly `pg_dump` CronJob and an `await-schema` initContainer; Docker Compose for
  a single machine; Prometheus metrics on a port that is deliberately not routed;
  `/healthz` and `/readyz` that distinguish liveness from readiness; and a schema
  check that refuses to serve against a database older than the binary.

### Known gaps

- **No Fever API yet**, so mobile clients cannot connect. Planned for 0.2.
- **Single user.** The schema is user-scoped throughout, but nothing creates a
  second account.
- **Blob replication is manual.** The nightly dump covers the database; the archive
  directory is yours to replicate. See
  [Back up and restore](docs/how-to/back-up-and-restore.md).
- **JavaScript-rendered sites are not archived.** No headless browser, by choice.

[Unreleased]: https://github.com/runlevel-six/tomekeeper/compare/v0.17.0...HEAD
[v0.17.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.16.0...v0.17.0
[v0.16.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.15.0...v0.16.0
[v0.15.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.14.0...v0.15.0
[v0.14.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.13.0...v0.14.0
[v0.13.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.12.1...v0.13.0
[v0.12.1]: https://github.com/runlevel-six/tomekeeper/compare/v0.12.0...v0.12.1
[v0.12.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.11.0...v0.12.0
[v0.11.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.10.1...v0.11.0
[v0.10.1]: https://github.com/runlevel-six/tomekeeper/compare/v0.10.0...v0.10.1
[v0.10.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.9.0...v0.10.0
[v0.9.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.8.0...v0.9.0
[v0.8.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.7.0...v0.8.0
[v0.7.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.6.0...v0.7.0
[v0.6.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.5.0...v0.6.0
[v0.5.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.4.0...v0.5.0
[v0.4.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.3.0...v0.4.0
[v0.3.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.2.0...v0.3.0
[v0.2.0]: https://github.com/runlevel-six/tomekeeper/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/runlevel-six/tomekeeper/releases/tag/v0.1.0
