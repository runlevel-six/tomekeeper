package server_test

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

func TestChoosingAPaletteAppliesIt(t *testing.T) {
	rd, _ := readingFixture(t)

	// Before: the default palette sets no attribute at all.
	if body := rd.body("/"); strings.Contains(body, "data-theme=") {
		t.Errorf("the default reader has a data-theme attribute:\n%s", firstLines(body))
	}

	rec := rd.do(http.MethodPost, "/settings", url.Values{"palette": {"verdant"}, "mode": {""}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings = %d\n%s", rec.Code, rec.Body.String())
	}

	// After: every page carries it, rendered server-side so there is no flash.
	for _, path := range []string{"/", "/all", "/feeds", "/settings"} {
		if body := rd.body(path); !strings.Contains(body, `data-theme="verdant"`) {
			t.Errorf("%s does not carry the chosen palette:\n%s", path, firstLines(body))
		}
	}
}

func TestForcingLightOrDarkIsIndependentOfThePalette(t *testing.T) {
	for _, mode := range []string{"light", "dark"} {
		t.Run(mode, func(t *testing.T) {
			rd, _ := readingFixture(t)

			if rec := rd.do(http.MethodPost, "/settings",
				url.Values{"palette": {"oxblood"}, "mode": {mode}}); rec.Code != http.StatusOK {
				t.Fatalf("POST /settings = %d", rec.Code)
			}

			want := `data-theme="oxblood-` + mode + `"`
			if body := rd.body("/"); !strings.Contains(body, want) {
				t.Errorf("the page does not carry %s:\n%s", want, firstLines(body))
			}
		})
	}
}

// The value goes straight into an HTML attribute, so it must be assembled from
// the known lists rather than taken from the form.
func TestAnUnknownPaletteIsRefused(t *testing.T) {
	rd, _ := readingFixture(t)

	rec := rd.do(http.MethodPost, "/settings", url.Values{
		"palette": {`" onload="alert(1)`},
		"mode":    {"dark"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings = %d", rec.Code)
	}

	body := rd.body("/")
	if strings.Contains(body, "onload") {
		t.Fatalf("an attacker-supplied palette reached the page:\n%s", firstLines(body))
	}
	if strings.Contains(body, "data-theme=") {
		t.Errorf("an unknown palette was stored rather than falling back to the default:\n%s", firstLines(body))
	}
}

// The palette is a property of the reader, not of the browser, so it has to
// survive a new session.
func TestThePaletteSurvivesSigningOutAndIn(t *testing.T) {
	rd, _ := readingFixture(t)

	if rec := rd.do(http.MethodPost, "/settings",
		url.Values{"palette": {"aegean"}, "mode": {""}}); rec.Code != http.StatusOK {
		t.Fatalf("POST /settings = %d", rec.Code)
	}

	login := postLogin(t, rd.h, "tome", testPassword)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("signing in again = %d", login.Code)
	}
	rd.jar = login.Result().Cookies()

	if body := rd.body("/"); !strings.Contains(body, `data-theme="aegean"`) {
		t.Errorf("the palette did not survive a new session:\n%s", firstLines(body))
	}
}

func firstLines(s string) string {
	lines := strings.SplitN(s, "\n", 4)
	if len(lines) > 3 {
		lines = lines[:3]
	}
	return strings.Join(lines, "\n")
}

// Every palette the settings page offers must have a stylesheet to go with it.
// A missing block is invisible in review and silent at runtime: the attribute is
// set, nothing matches it, and the reader gets the default while believing they
// chose something.
func TestEveryOfferedPaletteHasAStylesheet(t *testing.T) {
	css, err := os.ReadFile("static/tome.css")
	if err != nil {
		t.Fatalf("reading the stylesheet: %v", err)
	}
	sheet := string(css)

	for _, p := range store.Palettes {
		for _, variant := range []string{p.Palette, p.Palette + "-light", p.Palette + "-dark"} {
			if !strings.Contains(sheet, `[data-theme="`+variant+`"]`) {
				t.Errorf("palette %q is offered but the stylesheet defines no %q block",
					p.Name, variant)
			}
		}
	}
}
