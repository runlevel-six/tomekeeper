# Themes

Six palettes from the archive seal designs, plus the original neutral one.
Chosen per reader under **Settings**, and stored against the user rather than in
a cookie, so the choice follows you between your phone and your desk.

## How a palette is built

Each is three colors — a deep field, a metallic, and a parchment — arranged two
ways:

- **Light**: parchment behind deep ink, with a *deepened* metallic for links.
- **Dark**: the field behind parchment ink, with the metallic itself for links.

The palette and the light/dark decision are independent. A palette on its own
follows the system; `-light` and `-dark` variants do not.

## Why the metallics are darkened in light mode

Gold on parchment is about **1.7:1**. That is fine for a bookbinding, where the
gold is embossed and catches the light, and unreadable as body-size link text on
a screen.

So none of the values in the stylesheet were picked by eye. Each foreground is
darkened or lightened from its source color until it clears 4.5:1 — WCAG AA for
normal text — against its own background. The source colors below are the design
sheet's; the rendered ones are in `tome.css`.

## The palettes

| Palette | Field | Metal | Parchment |
|---|---|---|---|
| **Midnight** (`midnight`) | `#0F223D` | `#C8A15A` | `#F2E8D5` |
| **Royal Plum** (`plum`) | `#3C2348` | `#C39A54` | `#F1E7D8` |
| **Verdant Archive** (`verdant`) | `#234A3A` | `#C29A52` | `#E7D4AE` |
| **Oxblood** (`oxblood`) | `#5B1F26` | `#B46F43` | `#EFE2CF` |
| **Slate & Silver** (`slate`) | `#353A43` | `#9EA1A4` | `#EEE7DA` |
| **Aegean Bronze** (`aegean`) | `#0D4C5E` | `#A86D43` | `#EEE4D1` |

## Measured contrast

Every foreground against its own background, as rendered. All 48 pairs pass
WCAG AA (4.5:1); the body text figures are far above it.

| Palette | Light: text | Light: secondary | Light: links | Dark: text | Dark: secondary | Dark: links |
|---|---|---|---|---|---|---|
| Midnight | 13.77 | 5.82 | 4.52 | 14.15 | 6.68 | 6.99 |
| Royal Plum | 12.11 | 5.10 | 4.54 | 12.52 | 6.15 | 5.76 |
| Verdant Archive | 7.74 | 4.51 | 4.54 | 8.22 | 4.54 | 4.55 |
| Oxblood | 10.80 | 4.78 | 4.58 | 11.24 | 5.58 | 4.55 |
| Slate & Silver | 10.31 | 4.53 | 4.51 | 10.71 | 5.53 | 4.97 |
| Aegean Bronze | 8.59 | 4.52 | 4.58 | 8.99 | 4.75 | 4.56 |

The lowest figure anywhere is 4.51:1, which is deliberate rather than lucky: the
generator stops as soon as a color passes, so it changes the designed color as
little as it can get away with.

## How it is applied

Server-rendered onto `<html data-theme>` at render time. There is no JavaScript
involved and no flash of the wrong palette on load — which is why the preference
lives in the database and not in `localStorage`, since anything read by a script
after paint shows the wrong colors first.

The value is assembled from the known palette and mode lists rather than taken
from the form, so a hand-crafted POST cannot put an arbitrary string into an HTML
attribute.

## Adding one

1. Add the three source colors and derive the variables (see the generator note
   at the top of the themes section in `tome.css`).
2. Add `[data-theme="<key>"]`, `[data-theme="<key>-light"]` and
   `[data-theme="<key>-dark"]` blocks.
3. Add it to `store.Palettes`.

Step 3 without step 2 is the failure worth knowing about: the attribute is set,
nothing matches it, and the reader silently gets the default while believing they
chose something. `TestEveryOfferedPaletteHasAStylesheet` exists to catch exactly
that.

## See also

- [Configuration](configuration.md)
