# Tomekeeper documentation

Tomekeeper is a self-hosted feed aggregator that permanently archives what it
reads. It subscribes to RSS, Atom, and JSON feeds; for every item it fetches
the linked page, extracts the readable article, downloads the images, and
stores the result as files on disk that outlive the application.

The binary is `tome`.

> **Status: M3 (assets and the filesystem archive).** The service polls feeds,
> fetches the linked pages politely, extracts readable bodies, localizes the
> images, and writes each article as a standalone page that opens in a browser
> with nothing running. There is no web interface yet — that is M4 — so reading
> today means opening files or querying Postgres. Sections of this documentation
> appear as the milestones that introduce them land; nothing here describes
> behavior that does not exist.

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
- [Reprocess the archive](how-to/reprocess-the-archive.md)
- [Troubleshoot a failing feed](how-to/troubleshoot-a-failing-feed.md)

### Reference — information

Dry, complete descriptions of the machinery. Look things up here.

- [Configuration](reference/configuration.md) — every environment variable
- [CLI](reference/cli.md) — every subcommand, exit code, and HTTP endpoint
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
- [Why the filesystem is the archive](explanation/why-the-filesystem-is-the-archive.md)
- [Dependencies](explanation/dependencies.md) — every dependency, and why

## Quick orientation

| I want to… | Go to |
|---|---|
| Get it running for the first time | [Tutorial 1](tutorials/01-first-run.md) |
| Know what to set before starting the service | [Configuration](reference/configuration.md) |
| Know what `tome` can be told to do | [CLI](reference/cli.md) |
| Work out why a feed stopped producing articles | [Troubleshoot a failing feed](how-to/troubleshoot-a-failing-feed.md) |
| Understand the schema | [Data model](reference/data-model.md) |
| Understand why there are two Deployments | [Architecture](explanation/architecture.md) |
| Understand why duplicates collapse | [Why articles are the root entity](explanation/why-articles-are-the-root-entity.md) |
| Fix a site whose articles extract badly | [Add a domain rule](how-to/add-a-domain-rule.md) |
| Apply an extraction improvement to old articles | [Reprocess the archive](how-to/reprocess-the-archive.md) |
| Know what the archive costs on disk | [Storage layout](reference/storage-layout.md) |
| Read an article without this service running | [Why the filesystem is the archive](explanation/why-the-filesystem-is-the-archive.md) |
| Understand why a dependency is present | [Dependencies](explanation/dependencies.md) |
