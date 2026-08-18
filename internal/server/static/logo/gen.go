//go:build ignore

// Generates the archive seal — the SVG mark and every raster fallback — from one
// definition of the geometry.
//
// Run from this directory:
//
//	go run gen.go
//
// Build-tagged out of the binary: this is a design tool, not part of the server.
//
// One source for both formats on purpose. Hand-writing an SVG and then
// hand-drawing a matching PNG is how a favicon ends up subtly different from the
// header mark, and the difference is invisible until someone puts them side by
// side. Here the shapes are Go values, the SVG is printed from them, and the
// PNGs are rasterized from them.
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"strings"

	"golang.org/x/image/vector"
)

// The seal is drawn in a 64-unit square and scaled from there.
const side = 64.0

// Brand colors: the Midnight palette from the design sheet.
//
// Fixed rather than themed, unlike the header mark. A favicon is the
// application's identity in a tab strip full of other applications, and one that
// changed with the reader's palette would be a different icon on their phone
// than on their desk. The header mark inside the page does follow the theme —
// that one is decoration, this one is a name.
var (
	field = color.NRGBA{0x0F, 0x22, 0x3D, 0xFF} // deep navy
	metal = color.NRGBA{0xC8, 0xA1, 0x5A, 0xFF} // gold leaf
)

type point struct{ X, Y float64 }

type path []point

// The geometry, in drawing order.
//
// Each shape is a list of closed paths. A path wound opposite to the one before
// it cuts a hole, which is how the rings and the diamond outline are made: the
// rasterizer fills by non-zero winding, so a reversed inner path subtracts.
type shape struct {
	name  string
	paths []path
	fill  color.NRGBA
}

func circle(cx, cy, r float64, clockwise bool) path {
	const segments = 96
	p := make(path, 0, segments)
	for i := range segments {
		t := 2 * math.Pi * float64(i) / segments
		if !clockwise {
			t = -t
		}
		p = append(p, point{cx + r*math.Cos(t), cy + r*math.Sin(t)})
	}
	return p
}

func diamond(cx, cy, r float64, clockwise bool) path {
	p := path{{cx, cy - r}, {cx + r, cy}, {cx, cy + r}, {cx - r, cy}}
	if !clockwise {
		p[1], p[3] = p[3], p[1]
	}
	return p
}

func rect(x0, y0, x1, y1 float64) path {
	return path{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}}
}

// seal is the full mark: ring, compass studs, diamond frame, letter.
//
// simple drops the ring and the studs and enlarges the diamond, for sizes where
// four concentric elements become a smudge. Browsers pick per size from the
// <link rel="icon" sizes> list, which is exactly what that mechanism is for.
func seal(simple bool) []shape {
	const c = side / 2

	var shapes []shape

	// Weights are set for the smallest size each shape has to survive, not for
	// the 512 that looks best in a design review. At 32px a 2.4-unit ring is
	// under two pixels and starts to break up; at 16px it disappears entirely,
	// which is why the simple variant exists.
	diamondOuter, diamondInner := 20.5, 16.3
	// The letter, as (crossbar left, right, top, bottom, stem half-width).
	barL, barR, barT, barB, stem := 26.0, 38.0, 23.5, 28.5, 2.3
	letterBottom := 40.5

	if simple {
		// Everything heavier, not merely larger. At 16px one unit is a quarter
		// of a pixel: the full mark's 5-unit crossbar lands on 1.25px and reads
		// as a smudge inside the diamond rather than as a letter.
		diamondOuter, diamondInner = 27.0, 19.0
		barL, barR, barT, barB, stem = 25.0, 39.0, 22.0, 29.0, 3.25
		letterBottom = 42.0
	}

	// The medallion itself, opaque so the mark reads on any browser chrome,
	// light or dark. A transparent icon has to work against both and manages
	// neither.
	shapes = append(shapes, shape{
		name:  "field",
		paths: []path{circle(c, c, 31, true)},
		fill:  field,
	})

	if !simple {
		shapes = append(shapes, shape{
			name:  "outer-ring",
			paths: []path{circle(c, c, 29.5, true), circle(c, c, 26.3, false)},
			fill:  metal,
		})

		// Four points of the compass, as the small diamonds on the seal.
		var studs []path
		for _, a := range []float64{-90, 0, 90, 180} {
			t := a * math.Pi / 180
			studs = append(studs, diamond(c+29.5*math.Cos(t), c+29.5*math.Sin(t), 3, true))
		}
		shapes = append(shapes, shape{name: "studs", paths: studs, fill: metal})
	}

	shapes = append(shapes, shape{
		name:  "diamond",
		paths: []path{diamond(c, c, diamondOuter, true), diamond(c, c, diamondInner, false)},
		fill:  metal,
	})

	// The T. Drawn as two bars rather than set in a typeface, so the mark needs
	// no font to render and cannot shift when one is missing.
	shapes = append(shapes, shape{
		name: "letter",
		paths: []path{
			// Sized to sit inside the inner diamond with margin rather than to
			// fill the space. The first attempt was 18 wide and collided with
			// the frame, which at small sizes turned the whole center into one
			// gold blob.
			rect(barL, barT, barR, barB),             // crossbar
			rect(c-stem, barT, c+stem, letterBottom), // stem
		},
		fill: metal,
	})

	return shapes
}

// ---- SVG ------------------------------------------------------------------

func writeSVG(path string, shapes []shape) error {
	var b strings.Builder

	// width and height as well as viewBox, deliberately.
	//
	// Gecko draws nothing at all for an SVG favicon with no intrinsic size, and
	// caches that nothing — so the icon stays missing after the file is fixed
	// until the cache is cleared. viewBox alone is enough for an <img> in a page
	// and is not enough here.
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64" viewBox="0 0 64 64" role="img" aria-label="Tomekeeper">` + "\n")

	for _, s := range shapes {
		fill := fmt.Sprintf("#%02X%02X%02X", s.fill.R, s.fill.G, s.fill.B)
		b.WriteString(fmt.Sprintf(`  <path fill="%s" fill-rule="evenodd" d="`, fill))
		for _, p := range s.paths {
			for i, pt := range p {
				verb := "L"
				if i == 0 {
					verb = "M"
				}
				b.WriteString(fmt.Sprintf("%s%.2f %.2f", verb, pt.X, pt.Y))
				if i < len(p)-1 {
					b.WriteString(" ")
				}
			}
			b.WriteString("Z")
		}
		b.WriteString("\"/>\n")
	}

	b.WriteString("</svg>\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// ---- PNG ------------------------------------------------------------------

func rasterize(size int, shapes []shape) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := float64(size) / side

	for _, s := range shapes {
		r := vector.NewRasterizer(size, size)
		for _, p := range s.paths {
			r.MoveTo(float32(p[0].X*scale), float32(p[0].Y*scale))
			for _, pt := range p[1:] {
				r.LineTo(float32(pt.X*scale), float32(pt.Y*scale))
			}
			r.ClosePath()
		}
		r.Draw(dst, dst.Bounds(), image.NewUniform(s.fill), image.Point{})
	}
	return dst
}

func writePNG(path string, size int, shapes []shape, opaque bool) error {
	img := rasterize(size, shapes)

	if opaque {
		// iOS composites a home-screen icon onto black where it is transparent,
		// which puts a dark ring around a round mark. Flattening onto the field
		// color gives the square icon iOS is going to draw anyway.
		flat := image.NewNRGBA(img.Bounds())
		draw.Draw(flat, flat.Bounds(), image.NewUniform(field), image.Point{}, draw.Src)
		draw.Draw(flat, flat.Bounds(), img, image.Point{}, draw.Over)
		img = flat
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// writeMark emits the in-page mark as a template partial.
//
// A partial rather than a file the page links to, because an <img> cannot
// inherit currentColor and the mark has to follow whichever of the seven
// palettes the reader chose. Inlining it is what makes that free.
//
// Generated from the same geometry as the favicons so the two cannot drift. The
// field disc is dropped: inside the page the mark sits on the page's own
// background, and a navy disc would be a hole in every palette but one.
func writeMark(path string, shapes []shape) error {
	var b strings.Builder
	b.WriteString("{{define \"mark\"}}")
	b.WriteString(`<svg class="mark" width="26" height="26" viewBox="0 0 64 64" ` +
		`fill="currentColor" aria-hidden="true" focusable="false">`)

	for _, s := range shapes {
		if s.name == "field" {
			continue
		}
		b.WriteString(`<path fill-rule="evenodd" d="`)
		for _, p := range s.paths {
			for i, pt := range p {
				verb := "L"
				if i == 0 {
					verb = "M"
				}
				b.WriteString(fmt.Sprintf("%s%.2f %.2f", verb, pt.X, pt.Y))
				if i < len(p)-1 {
					b.WriteString(" ")
				}
			}
			b.WriteString("Z")
		}
		b.WriteString(`"/>`)
	}

	b.WriteString("</svg>{{end}}\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func main() {
	full, small := seal(false), seal(true)

	if err := writeMark("../../templates/mark.html", full); err != nil {
		panic(err)
	}
	fmt.Println("wrote ../../templates/mark.html")

	if err := writeSVG("../favicon.svg", full); err != nil {
		panic(err)
	}
	fmt.Println("wrote ../favicon.svg")

	for _, out := range []struct {
		path   string
		size   int
		opaque bool
		simple bool
	}{
		{"../favicon-16.png", 16, false, true}, // tab strips, where the ring is a smudge
		{"../favicon-32.png", 32, false, false},
		{"../favicon-180.png", 180, true, false}, // apple-touch-icon
		{"../favicon-192.png", 192, false, false},
		{"../favicon-512.png", 512, false, false},
	} {
		shapes := full
		if out.simple {
			shapes = small
		}
		if err := writePNG(out.path, out.size, shapes, out.opaque); err != nil {
			panic(err)
		}
		fmt.Println("wrote", out.path)
	}
}
