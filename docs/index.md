# Tomekeeper documentation

Tomekeeper is a self-hosted feed aggregator that permanently archives what it
reads. It subscribes to RSS, Atom, and JSON feeds; for every item it fetches
the linked page, extracts the readable article, downloads the images, and
stores the result as files on disk that outlive the application.

The binary is `tome`.

> **Status: M1 (ingest).** The service polls feeds on an adaptive schedule and
> records every article it finds, deduplicated across subscriptions. It does not
> yet fetch article pages or extract their text — that is M2 — and there is no
> web interface until M4. Sections of this documentation appear as the
> milestones that introduce them land; nothing here describes behavior that does
> not exist.

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

- [Troubleshoot a failing feed](how-to/troubleshoot-a-failing-feed.md)

### Reference — information

Dry, complete descriptions of the machinery. Look things up here.

- [Configuration](reference/configuration.md) — every environment variable
- [CLI](reference/cli.md) — every subcommand, exit code, and HTTP endpoint
- [Data model](reference/data-model.md) — the schema and its constraints

### Explanation — understanding

Why the system is built the way it is, including what was considered and
rejected. Design rationale lives here and nowhere else.

- [Architecture](explanation/architecture.md) — the shape of the system
- [Why articles are the root entity](explanation/why-articles-are-the-root-entity.md)
  — the decision the archive rests on
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
| Understand why a dependency is present | [Dependencies](explanation/dependencies.md) |
