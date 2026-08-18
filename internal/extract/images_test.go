package extract_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/extract"
)

var srcPattern = regexp.MustCompile(`<img[^>]*src="([^"]+)"`)

func imageSources(html string) []string {
	var out []string
	for _, m := range srcPattern.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

// The whole rung rests on telling a comic from the navigation around it, on a
// page where the navigation outnumbers the content twenty to one. This asserts
// which images are chosen, not merely that some were.
func TestPageImagesSelectsBySlug(t *testing.T) {
	const page = `<html><body>
	  <img src="/redesign/Top_Header.gif" alt="">
	  <img src="/redesign/ComicNav_Next.gif" alt="Next">
	  <img src="/images/logo.png" alt="Site">
	  <img src="/comics/strip/oots1347_a1b2c3.png" alt="Lowball Offer">
	  <img src="/Banners/Banner_Store.png" alt="">
	</body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://comics.example.com/comics/oots1347.html",
	})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if r.Name != extract.NamePageImages {
		t.Fatalf("extractor = %q, want %q", r.Name, extract.NamePageImages)
	}

	got := imageSources(r.HTML)
	if len(got) != 1 {
		t.Fatalf("kept %d images, want exactly the comic: %v", len(got), got)
	}
	if !strings.Contains(got[0], "oots1347_a1b2c3.png") {
		t.Errorf("kept %q, want the comic", got[0])
	}
	for _, chrome := range []string{"ComicNav", "logo", "Banner", "Top_Header"} {
		if strings.Contains(r.HTML, chrome) {
			t.Errorf("the body contains chrome image %q:\n%s", chrome, r.HTML)
		}
	}
}

// A multi-panel strip is several images under one slug, and all of them are the
// article.
func TestPageImagesKeepsEveryPanel(t *testing.T) {
	const page = `<html><body>
	  <img src="https://cdn.example.com/default/logo.png" alt="">
	  <img src="https://cdn.example.com/comics/design_hell/1.png" alt="">
	  <img src="https://cdn.example.com/comics/design_hell/2.jpg" alt="">
	  <img src="https://cdn.example.com/comics/design_hell/3.jpg" alt="">
	</body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://example.com/comics/design_hell",
	})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if got := imageSources(r.HTML); len(got) != 3 {
		t.Errorf("kept %d panels, want 3: %v", len(got), got)
	}
}

// The rung must not fire when there is nothing to match, or it would replace
// real articles with whatever picture happened to be on the page.
func TestPageImagesDoesNotFireWithoutASlugMatch(t *testing.T) {
	const page = `<html><body>
	  <img src="/images/hero.png" alt="">
	  <img src="/images/author-portrait.jpg" alt="">
	</body></html>`

	_, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://example.com/2026/some-article",
	})
	if err == nil {
		t.Error("extraction succeeded on a page with no slug-matching image, so unrelated pictures can be stored as articles")
	}
}

// A real article that happens to be short must keep its text.
func TestAShortArticleWithItsOwnImageIsNotReplaced(t *testing.T) {
	page := `<html><body><article><p>` +
		strings.Repeat("This is a short but genuine post about the subject at hand. ", 6) +
		`</p><img src="/2026/short-post/figure.png" alt="A figure"></article></body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://example.com/2026/short-post",
	})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if r.Name == extract.NamePageImages {
		t.Errorf("a short article carrying its own image was replaced by the image rung:\n%s", r.HTML)
	}
	if !strings.Contains(r.Text, "genuine post") {
		t.Errorf("the article's text was lost:\n%s", r.Text)
	}
}
