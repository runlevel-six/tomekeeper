# Vendored third-party assets

Committed rather than fetched at build time or loaded from a CDN. The interface brief asks for a
single binary with no build step that works offline, and all three of those fail
the moment a page references someone else's host.

There is a second reason, specific to this project: the archive is meant to still
open in ten years. A CDN reference is a dependency on a company still existing and
still serving that exact path, which is the same bet the files-are-the-archive principle exists to avoid making.

## htmx

| | |
|---|---|
| File | `htmx-2.0.9.min.js` |
| Version | 2.0.9 |
| Released | 2026-04-20 |
| Upstream | <https://github.com/bigskysoftware/htmx/releases/download/v2.0.9/htmx.min.js> |
| SHA-256 | `57d9191515339922bd1356d7b2d80b1ee3b29f1b3a2c65a078bb8b2e8fd9ae5f` |
| License | BSD-2-Clause, compatible with this project's AGPL-3.0 |

The version is in the filename so an upgrade is a new file and a changed template
reference rather than an invisible content swap, and so a browser cache cannot
serve the old one under the new name.

**2.x rather than 4.0.0-beta.** At the time of vendoring the GitHub API reported
`v4.0.0-beta5` as the latest release and did not mark it as a prerelease, which is
a trap: 2.0.9 is the newest stable line. A reader's archive is not the place to
run a beta.

### Upgrading

```sh
cd internal/server/static/vendor
curl -sSLO https://github.com/bigskysoftware/htmx/releases/download/vX.Y.Z/htmx.min.js
mv htmx.min.js htmx-X.Y.Z.min.js
sha256sum htmx-X.Y.Z.min.js      # record it in the table above
rm htmx-<old>.min.js
```

Then update the `<script src>` in `templates/base.html` and the table above. The
test in `server` asserts the referenced file exists, so a half-finished upgrade
fails the suite rather than the page.
