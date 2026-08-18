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

### How-to guides — goals

Recipes for a specific task, assuming you already know roughly what you are
doing.

- [Add a domain rule](how-to/add-a-domain-rule.md) — fixing a site that
  extracts badly
- [Install on Kubernetes](how-to/install-kubernetes.md) — the manifests in
  `deploy/`, and the handful of things that will confuse you once
- [Reprocess the archive](how-to/reprocess-the-archive.md)
- [Troubleshoot a failing feed](how-to/troubleshoot-a-failing-feed.md)

### Reference — information

Dry, complete descriptions of the machinery. Look things up here.

- [Configuration](reference/configuration.md) — every environment variable
- [CLI](reference/cli.md) — every subcommand, exit code, and HTTP endpoint
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
| Save a page nothing subscribed to | Paste it into **Saved** in the web interface |
| Use it on a phone, from the home screen | Install it from the browser's menu — see [CLI](reference/cli.md#installed-as-a-web-app) |
| Change how it looks | **Settings** in the web interface, or [Themes](reference/themes.md) |

**Keeping it healthy**

| I want to… | Go to |
|---|---|
| Work out why a feed stopped producing articles | [Troubleshoot a failing feed](how-to/troubleshoot-a-failing-feed.md) |
| Make it check the feeds right now | **Check all feeds now** on the Feeds page — see [Troubleshoot a failing feed](how-to/troubleshoot-a-failing-feed.md#watch-a-poll-happen) |
| Fix a site whose articles extract badly | [Add a domain rule](how-to/add-a-domain-rule.md) |
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
