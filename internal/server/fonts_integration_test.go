package server_test

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The vendored faces, and the three ways shipping a font fails without anyone
// noticing.
//
// All three degrade to the fallback stack rather than to an error, which is the
// problem: a missing file, a policy that forbids the fetch, and a preload pointing
// at last release's filename all look exactly like a reader who does not have
// Literata installed. Nothing in the interface says otherwise, so it has to be
// asserted here.

// cssURLs are the relative urls the stylesheet asks the browser to fetch.
var cssURLs = regexp.MustCompile(`url\(([^)]+)\)`)

func TestEveryFontTheStylesheetReferencesIsServed(t *testing.T) {
	rd, _ := readingFixture(t)

	css, err := os.ReadFile("static/tome.css")
	if err != nil {
		t.Fatalf("reading the stylesheet: %v", err)
	}

	matches := cssURLs.FindAllStringSubmatch(string(css), -1)
	if len(matches) == 0 {
		t.Fatal("the stylesheet references no files at all; this test is no longer checking anything")
	}

	var fonts int
	for _, m := range matches {
		ref := strings.Trim(m[1], `"' `)
		if !strings.HasSuffix(ref, ".woff2") {
			continue
		}
		fonts++

		// Relative to the stylesheet, which is served from /static/.
		path := "/static/" + ref
		rec := rd.get(path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d — the stylesheet references it, so the page renders in "+
				"the fallback serif with nothing to say why", path, rec.Code)
			continue
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s returned an empty body", path)
		}
		if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "font/woff2") {
			t.Errorf("GET %s served Content-Type %q, want font/woff2", path, got)
		}
	}

	// Four Literata faces and two Inter, at the time of writing. The floor is here
	// so that a stylesheet that stopped referencing fonts entirely fails loudly.
	if fonts < 6 {
		t.Errorf("the stylesheet references %d woff2 files, want at least 6", fonts)
	}
}

// A preload pointing at a file that no longer exists is worse than no preload: the
// browser fetches nothing useful, and the version in the filename means an upgrade
// changes the URL in two places or in neither.
func TestPreloadedFontsExistAndAreReferencedByTheStylesheet(t *testing.T) {
	rd, _ := readingFixture(t)

	css, err := os.ReadFile("static/tome.css")
	if err != nil {
		t.Fatalf("reading the stylesheet: %v", err)
	}

	preloads := regexp.MustCompile(`rel="preload" href="([^"]+)"`).FindAllStringSubmatch(rd.body("/"), -1)
	if len(preloads) == 0 {
		t.Fatal("no font is preloaded; the first paint of every article is the fallback")
	}

	for _, m := range preloads {
		path := m[1]
		if rec := rd.get(path); rec.Code != http.StatusOK {
			t.Errorf("preloaded %s = %d — a half-finished font upgrade", path, rec.Code)
		}
		if ref := strings.TrimPrefix(path, "/static/"); !strings.Contains(string(css), ref) {
			t.Errorf("%s is preloaded but the stylesheet never asks for it, so the download "+
				"is pure waste", path)
		}
	}
}

// Fonts are 320KB that never change, under names that carry their version. The
// five-minute policy the stylesheet needs would have made shipping them slower
// than the system stacks they replaced.
func TestVendoredAssetsAreCachedForALongTime(t *testing.T) {
	rd, _ := readingFixture(t)

	vendored := rd.get("/static/vendor/fonts/literata-5.3.0-latin-wght-normal.woff2")
	if vendored.Code != http.StatusOK {
		t.Fatalf("GET the vendored font = %d", vendored.Code)
	}
	cache := vendored.Header().Get("Cache-Control")
	if !strings.Contains(cache, "immutable") {
		t.Errorf("vendored font Cache-Control = %q, want it immutable", cache)
	}
	if strings.Contains(cache, "max-age=300") {
		t.Errorf("vendored font Cache-Control = %q, which revalidates every five minutes", cache)
	}

	// And the stylesheet keeps the short policy, because its name carries no
	// version and a year-long cache would strand a changed one in every browser.
	sheet := rd.get("/static/tome.css")
	if got := sheet.Header().Get("Cache-Control"); strings.Contains(got, "immutable") {
		t.Errorf("the stylesheet is cached as immutable (%q), but its name has no version in it", got)
	}
}

// Without font-src the fetch is refused by the policy and the page silently uses
// the fallback stack — the failure that is indistinguishable from success.
func TestThePolicyAllowsTheFontsItShips(t *testing.T) {
	rd, _ := readingFixture(t)

	policy := rd.get("/").Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "font-src 'self'") {
		t.Errorf("the content security policy does not allow this origin's fonts: %q", policy)
	}
	// 'self' and nothing else: a font host would put the reading experience back on
	// somebody else's uptime, and the archive is meant to outlive companies.
	for _, forbidden := range []string{"fonts.googleapis.com", "fonts.gstatic.com", "data:"} {
		if strings.Contains(policy, "font-src 'self' "+forbidden) {
			t.Errorf("the font policy admits %s", forbidden)
		}
	}
}
