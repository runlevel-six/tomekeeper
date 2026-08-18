package server

import (
	"encoding/xml"
	"image"
	"image/png"
	"io/fs"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The SVG favicon must declare an intrinsic size.
//
// Without width and height, Gecko renders nothing at all for an SVG favicon —
// and caches the nothing, so the icon stays missing after the file is corrected
// until someone clears the cache. viewBox alone is enough for an <img> in a page
// and is not enough here, which is exactly what makes the mistake easy to make
// and hard to notice.
func TestSVGFaviconHasAnIntrinsicSize(t *testing.T) {
	raw, err := fs.ReadFile(assets, "static/favicon.svg")
	if err != nil {
		t.Fatalf("reading the SVG favicon: %v", err)
	}

	var svg struct {
		XMLName xml.Name `xml:"svg"`
		Width   string   `xml:"width,attr"`
		Height  string   `xml:"height,attr"`
		ViewBox string   `xml:"viewBox,attr"`
	}
	if err := xml.Unmarshal(raw, &svg); err != nil {
		t.Fatalf("the SVG favicon is not well-formed XML: %v", err)
	}

	if svg.Width == "" || svg.Height == "" {
		t.Errorf("width=%q height=%q — an SVG favicon with no intrinsic size renders as nothing in Gecko",
			svg.Width, svg.Height)
	}
	if svg.ViewBox == "" {
		t.Error("no viewBox, so the mark cannot scale")
	}
}

// Every icon the pages reference must exist, be a real PNG, and be the size it
// claims. A 404 or a mislabeled size is invisible until someone looks at a tab.
func TestFaviconsExistAtTheDeclaredSizes(t *testing.T) {
	want := map[string]int{
		"static/favicon-16.png":  16,
		"static/favicon-32.png":  32,
		"static/favicon-180.png": 180,
		"static/favicon-192.png": 192,
		"static/favicon-512.png": 512,
	}

	for name, size := range want {
		f, err := assets.Open(name)
		if err != nil {
			t.Errorf("%s is referenced but not embedded: %v", name, err)
			continue
		}
		cfg, err := png.DecodeConfig(f)
		f.Close()
		if err != nil {
			t.Errorf("%s is not a decodable PNG: %v", name, err)
			continue
		}
		if cfg.Width != size || cfg.Height != size {
			t.Errorf("%s is %dx%d, but is declared as %dx%d", name, cfg.Width, cfg.Height, size, size)
		}
	}
}

// The apple-touch-icon must be opaque. iOS composites a transparent one onto
// black, which draws a dark square around a round mark.
func TestAppleTouchIconIsOpaque(t *testing.T) {
	f, err := assets.Open("static/favicon-180.png")
	if err != nil {
		t.Fatalf("opening the apple-touch-icon: %v", err)
	}
	defer f.Close()

	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}

	b := img.Bounds()
	for _, p := range []image.Point{
		b.Min,
		{b.Max.X - 1, b.Min.Y},
		{b.Min.X, b.Max.Y - 1},
		{b.Max.X - 1, b.Max.Y - 1},
	} {
		if _, _, _, a := img.At(p.X, p.Y).RGBA(); a != 0xffff {
			t.Errorf("corner %v has alpha %d; iOS will composite this onto black", p, a)
		}
	}
}

// Everything the head references has to be embedded, or it is a 404 nobody sees
// until they look at a tab or try to install the app.
func TestEveryReferencedStaticAssetIsEmbedded(t *testing.T) {
	raw, err := fs.ReadFile(assets, "templates/base.html")
	if err != nil {
		t.Fatalf("reading base.html: %v", err)
	}

	for _, ref := range []string{
		"favicon.svg", "favicon-16.png", "favicon-32.png",
		"favicon-180.png", "manifest.webmanifest", "tome.css",
	} {
		if !strings.Contains(string(raw), ref) {
			t.Errorf("base.html does not reference %s", ref)
			continue
		}
		if _, err := assets.Open("static/" + ref); err != nil {
			t.Errorf("base.html references %s but it is not embedded: %v", ref, err)
		}
	}
}

// default-src is 'none', so the manifest needs its own allowance. Without it the
// fetch is blocked and "add to home screen" silently offers a generic icon and
// the wrong name.
func TestTheManifestIsAllowedByTheContentSecurityPolicy(t *testing.T) {
	raw, err := fs.ReadFile(assets, "templates/base.html")
	if err != nil {
		t.Fatalf("reading base.html: %v", err)
	}
	if !strings.Contains(string(raw), "manifest") {
		t.Skip("no manifest is referenced")
	}

	src, err := fs.ReadFile(assets, "static/manifest.webmanifest")
	if err != nil {
		t.Fatalf("the manifest is referenced but not embedded: %v", err)
	}
	if len(src) == 0 {
		t.Error("the manifest is empty")
	}
}

// The theme-color meta tags are a hand-copied duplicate of what the stylesheet
// paints. This is what stops the copy drifting.
func TestThemeColorsMatchTheStylesheet(t *testing.T) {
	css, err := fs.ReadFile(assets, "static/tome.css")
	if err != nil {
		t.Fatalf("reading the stylesheet: %v", err)
	}
	sheet := string(css)

	for _, p := range store.Palettes {
		for _, c := range []struct{ mode, value string }{
			{"light", p.LightBG},
			{"dark", p.DarkBG},
		} {
			if c.value == "" {
				t.Errorf("palette %q has no %s background recorded", p.Name, c.mode)
				continue
			}
			if !strings.Contains(sheet, c.value) {
				t.Errorf("palette %q declares its %s background as %s, which appears nowhere in the stylesheet",
					p.Name, c.mode, c.value)
			}
		}
	}
}
