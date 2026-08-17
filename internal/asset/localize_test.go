package asset_test

import (
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/asset"
)

// resolveAll localizes everything, recording what it was asked for.
func resolveAll(fetched *[]string) asset.Resolver {
	return func(src string) (string, bool) {
		*fetched = append(*fetched, src)
		return "assets/sha256/aa/bb/" + sha(src) + ".avif", true
	}
}

// sha is a stand-in address, since Localize does not care what the path is.
func sha(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 12 {
		out = out[len(out)-12:]
	}
	return out
}

func TestLocalizeRewritesImages(t *testing.T) {
	var fetched []string

	body := `<p>Before.</p><img src="https://example.com/photo.jpg" alt="A photo"><p>After.</p>`

	got, outcome := asset.Localize(body, resolveAll(&fetched))

	if outcome.Found != 1 || outcome.Localized != 1 || outcome.Failed != 0 {
		t.Errorf("outcome = %+v, want 1 found and 1 localized", outcome)
	}
	if len(fetched) != 1 || fetched[0] != "https://example.com/photo.jpg" {
		t.Errorf("fetched %v, want the one image", fetched)
	}
	if !strings.Contains(got, `src="/assets/sha256/`) {
		t.Errorf("the image was not rewritten:\n%s", got)
	}
	if strings.Contains(got, "https://example.com/photo.jpg") {
		t.Errorf("the original URL survived:\n%s", got)
	}
	// The surrounding article must be untouched.
	for _, want := range []string{"Before.", "After.", `alt="A photo"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the body lost %q:\n%s", want, got)
		}
	}
}

// §5.5: pick one candidate and drop the rest. Keeping a srcset would send a
// reader's browser back to the origin for a picture already stored here.
func TestLocalizeFlattensSrcset(t *testing.T) {
	var fetched []string

	body := `<img src="https://example.com/small.jpg" ` +
		`srcset="https://example.com/small.jpg 400w, https://example.com/big.jpg 1500w, https://example.com/huge.jpg 4000w" ` +
		`sizes="(max-width: 600px) 400px, 1500px">`

	got, outcome := asset.Localize(body, resolveAll(&fetched))

	if outcome.Localized != 1 {
		t.Errorf("localized %d images, want 1", outcome.Localized)
	}
	if len(fetched) != 1 {
		t.Fatalf("fetched %d images, want 1: %v", len(fetched), fetched)
	}
	// Nearest to 1600, not the largest: fetching the 4000px original only to
	// downscale it wastes bandwidth on both ends.
	if fetched[0] != "https://example.com/big.jpg" {
		t.Errorf("fetched %q, want the 1500w candidate as nearest to the target", fetched[0])
	}
	if strings.Contains(got, "srcset") {
		t.Errorf("srcset survived:\n%s", got)
	}
	if strings.Contains(got, "sizes") {
		t.Errorf("sizes survived:\n%s", got)
	}
}

func TestLocalizeFlattensPicture(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantFetch string
	}{
		{
			name: "source with srcset wins over the fallback img",
			body: `<picture>` +
				`<source type="image/webp" srcset="https://example.com/modern.webp 1500w">` +
				`<img src="https://example.com/fallback.jpg" alt="A photo">` +
				`</picture>`,
			wantFetch: "https://example.com/modern.webp",
		},
		{
			name:      "source with a plain src",
			body:      `<picture><source src="https://example.com/a.avif"><img src="https://example.com/b.jpg"></picture>`,
			wantFetch: "https://example.com/a.avif",
		},
		{
			name:      "picture with only sources",
			body:      `<picture><source srcset="https://example.com/only.avif 1600w"></picture>`,
			wantFetch: "https://example.com/only.avif",
		},
		{
			name: "the img's own srcset is respected over the sources",
			body: `<picture>` +
				`<source srcset="https://example.com/source.webp 800w">` +
				`<img srcset="https://example.com/chosen.jpg 1600w" src="https://example.com/fallback.jpg">` +
				`</picture>`,
			wantFetch: "https://example.com/chosen.jpg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fetched []string

			got, outcome := asset.Localize(tt.body, resolveAll(&fetched))

			if outcome.Localized != 1 {
				t.Errorf("localized %d, want 1\n%s", outcome.Localized, got)
			}
			if len(fetched) != 1 || fetched[0] != tt.wantFetch {
				t.Errorf("fetched %v, want [%s]", fetched, tt.wantFetch)
			}
			// The <picture> element has nothing left to decide once the
			// archive holds one file in one format.
			if strings.Contains(got, "<picture") || strings.Contains(got, "<source") {
				t.Errorf("picture markup survived:\n%s", got)
			}
			if !strings.Contains(got, "<img") {
				t.Errorf("no img element remains:\n%s", got)
			}
		})
	}
}

// A failure keeps the original URL: a hotlinked image that still loads beats a
// broken one, and the article is marked partial so the gap is visible.
func TestLocalizeFailureKeepsAbsoluteURL(t *testing.T) {
	body := `<img src="https://example.com/gone.jpg">`

	got, outcome := asset.Localize(body, func(string) (string, bool) { return "", false })

	if outcome.Found != 1 || outcome.Failed != 1 || outcome.Localized != 0 {
		t.Errorf("outcome = %+v, want 1 found and 1 failed", outcome)
	}
	if !strings.Contains(got, "https://example.com/gone.jpg") {
		t.Errorf("the original URL was dropped, leaving nothing to show:\n%s", got)
	}
}

// Data URIs carry their own bytes; there is nothing to fetch or rewrite.
func TestLocalizeLeavesDataURIs(t *testing.T) {
	const dataURI = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
	var fetched []string

	got, outcome := asset.Localize(`<img src="`+dataURI+`">`, resolveAll(&fetched))

	if len(fetched) != 0 {
		t.Errorf("fetched %v, want nothing for a data URI", fetched)
	}
	if outcome.Localized != 0 || outcome.Failed != 0 {
		t.Errorf("outcome = %+v, want a data URI counted as neither", outcome)
	}
	if !strings.Contains(got, dataURI) {
		t.Errorf("the data URI was damaged:\n%s", got)
	}
}

// The same image used twice in one article is fetched once by the caller's
// resolver — Localize asks for each reference, and the resolver's own cache
// is what deduplicates. This test pins the contract.
func TestLocalizeAsksForEveryReference(t *testing.T) {
	var fetched []string

	body := `<img src="https://example.com/same.jpg"><p>Text.</p><img src="https://example.com/same.jpg">`

	_, outcome := asset.Localize(body, resolveAll(&fetched))

	if outcome.Found != 2 {
		t.Errorf("Found = %d, want 2 references", outcome.Found)
	}
	if len(fetched) != 2 {
		t.Errorf("the resolver was called %d times, want once per reference", len(fetched))
	}
}

func TestLocalizeEmptyBody(t *testing.T) {
	got, outcome := asset.Localize("", func(string) (string, bool) {
		t.Error("the resolver was called for an empty body")
		return "", false
	})
	if got != "" || outcome.Found != 0 {
		t.Errorf("Localize(\"\") = %q, %+v", got, outcome)
	}
}

func TestLocalizeBodyWithNoImages(t *testing.T) {
	body := `<p>Just words, and <a href="https://example.com">a link</a>.</p>`

	got, outcome := asset.Localize(body, func(string) (string, bool) {
		t.Error("the resolver was called for a body with no images")
		return "", false
	})

	if outcome.Found != 0 {
		t.Errorf("Found = %d, want 0", outcome.Found)
	}
	if !strings.Contains(got, "Just words") {
		t.Errorf("the body was damaged:\n%s", got)
	}
}
