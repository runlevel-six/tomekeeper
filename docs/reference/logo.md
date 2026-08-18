# The mark and the lockup

There are two drawings, and reaching for the wrong one is the mistake this page
exists to prevent.

| | **The mark** | **The lockup** |
|---|---|---|
| What | An archive seal: a gold diamond and a serif T on a deep navy field, with a compass ring | The tome and its sigil, plus the "Tomekeeper" wordmark |
| Form | Generated vector, one monochrome glyph | Raster, full color, its own navy-and-gold palette |
| Color | Inherits `currentColor`, so one drawing serves all seven palettes | Fixed. It brings its own colors and would fight six of the seven |
| Where | The header, the favicons, the installed app icon — everywhere Tomekeeper is already *running* | The README and the sign-in page — where Tomekeeper is being *introduced* |

Everything below is about the mark unless it says otherwise.

## One source, several files

Everything is generated from one definition of the geometry in
`internal/server/static/logo/gen.go`:

```sh
cd internal/server/static/logo && go run gen.go
```

It is build-tagged `ignore`, so it never compiles into the binary.

Hand-writing an SVG and then hand-drawing a matching PNG is how a favicon ends up
subtly different from the header mark — a difference invisible until someone puts
them side by side. Here the shapes are Go values, the SVG is printed from them,
and the PNGs are rasterized from them.

| File | Size | Used for |
|---|---|---|
| `favicon.svg` | scalable | Browsers that prefer SVG |
| `favicon-16.png` | 16 | Tab strips — **a separate, simplified drawing** |
| `favicon-32.png` | 32 | Bookmarks, higher-density tabs |
| `favicon-180.png` | 180 | `apple-touch-icon`, **opaque** |
| `favicon-192.png`, `favicon-512.png` | 192, 512 | Web app manifest |
| `templates/mark.html` | — | The in-page mark, as a template partial |

## Four things that fail silently

Each of these produces no error anywhere. That is what makes them worth writing
down, and each has a test.

**An SVG favicon needs `width` and `height`, not just `viewBox`.** Gecko renders
nothing at all without an intrinsic size — and caches the nothing, so the icon
stays missing after the file is fixed until the cache is cleared. `viewBox` alone
is sufficient for an `<img>` in a page, which is precisely why the omission looks
harmless.

**16px needs its own drawing.** The full mark has four concentric elements; at
16px, one unit of the 64-unit grid is a quarter of a pixel, and the ring and
studs become a smudge around a shape you cannot identify. The small variant drops
the ring, enlarges the diamond, and thickens the letter — it is heavier, not
merely smaller. Browsers choose per size from the `sizes` attributes.

**The apple-touch-icon must be opaque.** iOS composites a transparent icon onto
black, drawing a dark square around a round mark. This one is flattened onto the
navy field.

**The manifest needs `manifest-src` in the CSP.** `default-src` is `'none'`, so
the manifest fetch is blocked outright — and the symptom is not an error anyone
sees: "add to home screen" simply offers a generic icon and the wrong name.

## Themed, or not

The **favicon is fixed** to the Midnight palette. It is the application's
identity in a tab strip full of other applications, and one that changed with the
reader's palette would be a different icon on their phone than on their desk.

The **in-page mark follows the theme**. It is inlined as a template partial
rather than linked as a file, because an `<img>` cannot inherit `currentColor` —
inlining is what lets one drawing serve all seven palettes for free.

Mobile browser chrome is colored by two `theme-color` meta tags with
`prefers-color-scheme` media queries, because a palette left on `auto` does not
choose between its halves until it meets a system preference, which happens after
the page was rendered.

## The lockup

Raster, and not generated from anything — it is artwork rather than geometry.

| File | Size | Used for |
|---|---|---|
| `docs/assets/logo-master.png` | 2172 × 724 | The master. Never served to a browser; it is here so the other two can be regenerated. |
| `docs/assets/logo.png` | 1200 × 400 | What the README shows. |
| `internal/server/static/wordmark.png` | 640 × 213 | Compiled into the binary and shown on the sign-in page. |

The two derived files are checked in. `wordmark.png` has to be, because `go:embed`
needs it at build time and the whole point of the binary is that it deploys as one
file; `logo.png` has to be, because a README renders from the repository.

Regenerate the smaller two from the master rather than resizing a resize — scale
onto an `image.NewNRGBA` of the target size with
`golang.org/x/image/draw`'s `CatmullRom`, then encode at
`png.BestCompression`. The 640 is twice its display width so it stays sharp on a
phone.

Two things about it worth knowing:

**It is a heading, not decoration.** On the sign-in page it carries
`alt="Tomekeeper"` and *is* the `<h1>`, rather than sitting beside a heading that
repeats the name. One name for a screen reader, and the word still appears if the
image does not load.

**It is never recolored and never themed.** The seal is monochrome so it can
follow the reader's palette; this one is a painting. If a page needs a brand mark
that matches the palette, it needs the seal.

## See also

- [Themes](themes.md)
