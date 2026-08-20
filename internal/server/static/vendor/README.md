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

## Fonts

`fonts/`, six woff2 files and the two licenses they travel under. Prose is set in
**Literata**, the interface in **Inter**.

Shipped rather than named in a stack, because a stack starting at `ui-serif` means
the reading experience is whatever serif the reader's operating system nominates —
Georgia on a Mac, and on Windows and much of Linux something markedly worse. That
is not a thing a reading tool can leave to chance. The system stacks stay behind
them as fallbacks.

| | |
|---|---|
| Files | `fonts/literata-5.3.0-latin{,-ext}-wght-{normal,italic}.woff2`, `fonts/inter-5.3.0-latin{,-ext}-wght-normal.woff2` |
| Version | Fontsource 5.3.0 packages — Literata 3.103, Inter 4.1 |
| Upstream | `https://registry.npmjs.org/@fontsource-variable/literata/-/literata-5.3.0.tgz` and `.../@fontsource-variable/inter/-/inter-5.3.0.tgz` |
| License | OFL-1.1 both, in `fonts/literata-OFL.txt` and `fonts/inter-OFL.txt`, compatible with this project's AGPL-3.0 |

SHA-256, of the files as vendored:

```
9adbeac5b167fe5ad6c49d9e29aa0c76e2f1bb3b46bf4ebf12a9eca7d3525384  literata-5.3.0-latin-wght-normal.woff2
ab198d6616c7cc966f26a4a5b28a3977dc47439640f09d9b3361226bd465c404  literata-5.3.0-latin-wght-italic.woff2
46792f7cd10b28fb512161caeaaa11b049feb2221ccaa72bb4d4ccc286199155  literata-5.3.0-latin-ext-wght-normal.woff2
ba0e6a12e2f0a1f5d455c23c47068b71f9898c571dcb92dab385294ebdae3e74  literata-5.3.0-latin-ext-wght-italic.woff2
3100e775e8616cd2611beecfa23a4263d7037586789b43f035236a2e6fbd4c62  inter-5.3.0-latin-wght-normal.woff2
34b9c504cab7a73e37b746343a449132e56cf7b5481af2cb81dc74dcff25c956  inter-5.3.0-latin-ext-wght-normal.woff2
```

### Fontsource rather than the foundry files

Both fonts publish TTFs; these are the woff2 subsets Fontsource builds from them.
Taking those means no `fonttools` in the build, no woff2 compressor to run by hand,
and a `unicode-range` per file that came from the same tool that cut the subsets.
The alternative was shipping full-charset TTFs at three or four times the bytes.

### `wght` and not `opsz`, which is a real trade

Literata has an optical-size axis, and this deliberately does not ship it. The
two-axis file is 110KB against 52KB for the weight-only one — 2.1× for both roman
and italic — and the axis earns that at display sizes and at 8pt, neither of which
this interface has. Reading text runs 17–21px and headings reach 35px, where the
difference is subtle at best.

Swapping it back is two filenames per face: the `-opsz-` files are in the same
package, and `font-optical-sizing: auto` then does the rest.

Latin and Latin-Extended only. Cyrillic, Greek and Vietnamese subsets exist in the
package and are not shipped: an article in those scripts renders in the fallback
serif, which is the right trade until somebody subscribes to one.

### Upgrading

```sh
cd internal/server/static/vendor/fonts
V=X.Y.Z
curl -sSL "https://registry.npmjs.org/@fontsource-variable/literata/-/literata-$V.tgz" | tar xz
for s in latin latin-ext; do for st in normal italic; do
  mv "package/files/literata-$s-wght-$st.woff2" "literata-$V-$s-wght-$st.woff2"
done; done
cp package/LICENSE literata-OFL.txt && rm -rf package   # and the same for inter
sha256sum ./*.woff2                                     # record above
rm ./*-<old version>-*.woff2
```

Then update the `url()`s in `static/tome.css`, the two `rel="preload"` links in
`templates/base.html`, the path in `scripts/smoke.sh`, and the table above.
`fonts_integration_test.go` covers every one of those except the smoke test: it
asserts each referenced file is actually served, that each preload is a file the
stylesheet asks for, and that the policy still admits them — because all three
failures degrade silently to the fallback stack and look identical to a font
nobody installed.
