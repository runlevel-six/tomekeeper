# Tomekeeper documentation

Tomekeeper is a self-hosted feed aggregator that permanently archives what it
reads. It subscribes to RSS, Atom, and JSON feeds; for every item it fetches
the linked page, extracts the readable article, downloads the images, and
stores the result as files on disk that outlive the application.

The binary is `tome`.

> **Status: M0 (skeleton).** The service starts, is configured, logs, and
> answers health probes. It does not yet poll feeds or store anything. Sections
> of this documentation appear as the milestones that introduce them land —
> nothing here describes behavior that does not exist.

## The four kinds of document

This documentation follows [Diátaxis](https://diataxis.fr). Each page serves
exactly one need, and mixing them is the failure this structure prevents.

### Tutorials — learning

Guided lessons that take you from nothing to a working result. Start here if
you are new.

*Arrives with M1–M2, once there is a feed to read.*

### How-to guides — goals

Recipes for a specific task, assuming you already know roughly what you are
doing.

*Arrives with M1 onward.*

### Reference — information

Dry, complete descriptions of the machinery. Look things up here.

- [Configuration](reference/configuration.md) — every environment variable
- [CLI](reference/cli.md) — every subcommand, exit code, and HTTP endpoint

### Explanation — understanding

Why the system is built the way it is, including what was considered and
rejected. Design rationale lives here and nowhere else.

- [Architecture](explanation/architecture.md) — the shape of the system
- [Dependencies](explanation/dependencies.md) — every dependency, and why

## Quick orientation

| I want to… | Go to |
|---|---|
| Know what to set before starting the service | [Configuration](reference/configuration.md) |
| Know what `tome` can be told to do | [CLI](reference/cli.md) |
| Understand why there are two Deployments | [Architecture](explanation/architecture.md) |
| Understand why a dependency is present | [Dependencies](explanation/dependencies.md) |
