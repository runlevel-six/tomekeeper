# Tomekeeper documentation

Tomekeeper is a self-hosted feed aggregator that permanently archives what it
reads. It subscribes to RSS, Atom, and JSON feeds; for every item it fetches
the linked page, extracts the readable article, downloads the images, and
stores the result as files on disk that outlive the application.

The binary is `tome`.

> **Status: early, and usable.** The service polls feeds, fetches the linked
> pages politely, extracts readable bodies, localizes the images, writes each
> article as a standalone page that opens in a browser with nothing running — and
> serves a web interface to read it in, with full-text search across the whole
> archive, browsing by category, and Kubernetes manifests that stand the whole
> thing up including its database. It installs to a home screen, where it draws
> its own navigation because a standalone window has none. Sections of this
> documentation appear as the milestones that introduce them land; nothing here
> describes behavior that does not exist.

## The four kinds of document

This documentation follows [Diátaxis](https://diataxis.fr). Each page serves
exactly one need, and mixing them is the failure this structure prevents.

### Tutorials — learning

Guided lessons that take you from nothing to a working result. Start here if
you are new.

- [1. Your first run](tutorials/01-first-run.md) — database to collected
  articles in about 20 minutes
- [2. Bring your reading with you](tutorials/02-import-your-feeds.md) — your
  subscriptions and your saved articles, and what does not survive the move

### How-to guides — goals

Recipes for a specific task, assuming you already know roughly what you are
doing.

- [Add a domain rule](how-to/add-a-domain-rule.md) — fixing a site that
  extracts badly
- [Add a reader](how-to/add-a-reader.md) — an account for somebody else, without
  learning their password
- [Back up and restore](how-to/back-up-and-restore.md) — the database and the
  files are two backups, and one of them is not automatic
- [Change how often feeds are checked](how-to/change-how-often-feeds-are-checked.md)
  — a cadence for everything, an override for one feed, and the floor neither can
  cross
- [Cut a release](how-to/cut-a-release.md) — the tag is the release, and what the
  version number promises an operator
- [Export everything](how-to/export-everything.md) — a portable copy of the whole
  archive, and what a round trip preserves
- [Install with Docker Compose](how-to/install-docker-compose.md) — one machine,
  no Kubernetes, nothing to build
- [Import from Wallabag](how-to/import-from-wallabag.md) — a read-later library,
  and what the report is telling you before you commit to it
- [Install on Kubernetes](how-to/install-kubernetes.md) — the manifests in
  `deploy/`, and the handful of things that will confuse you once
- [Reprocess the archive](how-to/reprocess-the-archive.md)
- [Troubleshoot a failing feed](how-to/troubleshoot-a-failing-feed.md)

### Reference — information

Dry, complete descriptions of the machinery. Look things up here.

- [Configuration](reference/configuration.md) — every environment variable
- [CLI](reference/cli.md) — every subcommand, exit code, and HTTP endpoint, plus
  the web interface's own routes
- [Metrics](reference/metrics.md) — every metric, and why they are on their own port
- [The mark and the lockup](reference/logo.md) — two drawings, which to use where, and four things about icons that fail silently
- [Themes](reference/themes.md) — the six palettes, and the contrast measurements
- [Retention](reference/retention.md) — what expires, what never does, and why
  it asks about every reader
- [Data model](reference/data-model.md) — the schema and its constraints
- [Storage layout](reference/storage-layout.md) — the on-disk archive
- [Export format](reference/export-format.md) — what `meta.json` contains

### Explanation — understanding

Why the system is built the way it is, including what was considered and
rejected. Design rationale lives here and nowhere else.

- [Architecture](explanation/architecture.md) — the shape of the system
- [Why articles are the root entity](explanation/why-articles-are-the-root-entity.md)
  — the decision the archive rests on
- [Extraction and versioning](explanation/extraction-and-versioning.md) — why
  bodies are regenerable and raw pages are kept
- [Politeness and rate limiting](explanation/politeness-and-rate-limiting.md)
- [What this deliberately will not do](explanation/non-goals.md) — the refusals,
  and the reasoning, so the arguments are had once
- [Scoping and access control](explanation/scoping-and-access-control.md) —
  one user today, several later, and what is kept true meanwhile
- [Why the filesystem is the archive](explanation/why-the-filesystem-is-the-archive.md)
- [Dependencies](explanation/dependencies.md) — every dependency, and why

## Quick orientation

**Getting it running**

| I want to… | Go to |
|---|---|
| Get it running for the first time | [Tutorial 1](tutorials/01-first-run.md) |
| Run this on a cluster | [Install on Kubernetes](how-to/install-kubernetes.md) |
| Know what to set before starting the service | [Configuration](reference/configuration.md) |

**Reading with it**

| I want to… | Go to |
|---|---|
| Know which keys do what | [CLI](reference/cli.md#keyboard) |
| Read only the comics, or only the tech feeds | **Categories** in the web interface — see [CLI](reference/cli.md#web-interface) |
| See what is new in one folder, and clear just that folder | The category links above **Unread** — see [CLI](reference/cli.md#narrowing-a-list-to-one-category) |
| Save a page nothing subscribed to | Paste it into **Saved** in the web interface |
| Use it on a phone, from the home screen | Install it from the browser's menu — see [CLI](reference/cli.md#installed-as-a-web-app) |
| Read it in an RSS app on my phone | [Connect a mobile client](how-to/connect-a-mobile-client.md) |
| Know exactly what the sync protocol does | [Fever API](reference/fever-api.md) |
| Change how it looks | **Settings** in the web interface, or [Themes](reference/themes.md) |

**Keeping it healthy**

| I want to… | Go to |
|---|---|
| Give somebody else an account | [Add a reader](how-to/add-a-reader.md) |
| Reset a forgotten password, or change my own | [Add a reader](how-to/add-a-reader.md#resetting-a-forgotten-password) |
| Work out why a feed stopped producing articles | [Troubleshoot a failing feed](how-to/troubleshoot-a-failing-feed.md) |
| Correct a feed's address, re-file it, or stop polling it | **Edit** on its row on the Feeds page — see [CLI](reference/cli.md#post-feedsidedit--change-one-subscription) |
| Get rid of a subscription, or one an import listed twice | **Edit → Unsubscribe** — see [Troubleshoot a failing feed](how-to/troubleshoot-a-failing-feed.md#when-the-same-feed-is-subscribed-to-twice) |
| Find one feed among hundreds, or list the broken ones first | Sort and filter the Feeds page — see [CLI](reference/cli.md#sorting-and-filtering-the-feed-list) |
| Make it check the feeds right now | **Check all feeds now** on the Feeds page — see [Troubleshoot a failing feed](how-to/troubleshoot-a-failing-feed.md#watch-a-poll-happen) |
| Fix a site whose articles extract badly | [Add a domain rule](how-to/add-a-domain-rule.md) |
| Archive a site that builds its pages in JavaScript | [Enable headless rendering](how-to/enable-headless-rendering.md) |
| Apply an extraction improvement to old articles | [Reprocess the archive](how-to/reprocess-the-archive.md) |
| Know what the archive costs on disk | [Storage layout](reference/storage-layout.md) |
| Stop the archive growing forever | [Retention](reference/retention.md) |
| Scrape it with Prometheus | [Metrics](reference/metrics.md) |
| Know what `tome` can be told to do | [CLI](reference/cli.md) |

**Understanding it**

| I want to… | Go to |
|---|---|
| Understand why there are two Deployments | [Architecture](explanation/architecture.md) |
| Understand the schema | [Data model](reference/data-model.md) |
| Understand why duplicates collapse | [Why articles are the root entity](explanation/why-articles-are-the-root-entity.md) |
| Read an article without this service running | [Why the filesystem is the archive](explanation/why-the-filesystem-is-the-archive.md) |
| Know what one reader can see of another's archive | [Scoping and access control](explanation/scoping-and-access-control.md) |
| Know why extraction has versions | [Extraction and versioning](explanation/extraction-and-versioning.md) |
| Know what this does to the sites it reads | [Politeness and rate limiting](explanation/politeness-and-rate-limiting.md) |
| Understand why a dependency is present | [Dependencies](explanation/dependencies.md) |
| Know what `meta.json` holds | [Export format](reference/export-format.md) |
