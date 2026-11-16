package extract_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/extract"
)

// articleParagraphs is enough prose for any rung to accept on its own terms.
func articleParagraphs() string {
	var b strings.Builder
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&b, "<p>Paragraph %d of the report. The committee met on a Tuesday and "+
			"spent most of the afternoon on the budget, which had grown again, and on the "+
			"question of who would pay for the new roof before the winter set in.</p>\n", i)
	}
	return b.String()
}

// An author box that ships, hidden inline, every post its author has written. The
// extractor kept the name from each entry and the article ended with the byline once
// per post — a thousand times on the page this was found on.
func TestHiddenAuthorArchiveIsNotPartOfTheArticle(t *testing.T) {
	var archive strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&archive, `<li><div class="entry"><div class="person"><div itemprop="name">Avery Quill</div></div>`+
			`<a href="/old/%d"><span>Old headline number %d</span></a></div></li>`, i, i)
	}

	page := `<!DOCTYPE html><html><head><title>The Roof</title></head><body>
<article>
<h1>The Roof</h1>
<p class="byline">By Avery Quill</p>
` + articleParagraphs() + `
<div class="author-box"><h5>Avery Quill</h5><p>Avery writes about local government.</p>
<div class="related" style="display: none;"><ul>` + archive.String() + `</ul></div></div>
</article>
</body></html>`

	got, err := extract.New().Extract(extract.Input{RawHTML: []byte(page), URL: "https://news.example.com/2026/11/the-roof"})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if !strings.Contains(got.Text, "who would pay for the new roof") {
		t.Errorf("article text missing:\n%s", got.Text)
	}
	for _, s := range []string{got.Text, got.HTML} {
		if n := strings.Count(s, "Avery Quill"); n > 2 {
			t.Errorf("author name appears %d times, want at most 2 (byline and author box)", n)
		}
		if strings.Contains(s, "Old headline number") {
			t.Errorf("hidden archive leaked into the body")
		}
	}
}

// A lightbox keeps a second copy of each caption next to the image for its zoomed view,
// hidden by the site's stylesheet rather than inline. Only the figcaption survives, and
// the image keeps its alt and hover text.
func TestFigureKeepsOneCaption(t *testing.T) {
	const caption = "The north wall, photographed in March before the scaffolding went up."
	page := `<!DOCTYPE html><html><head><title>The Roof</title></head><body>
<div class="post-content">
` + articleParagraphs() + `
<figure>
  <div class="lightbox">
    <a href="https://news.example.com/img/wall.jpg"><img src="https://news.example.com/img/wall.jpg" alt="A brick wall" title="Taken from the car park"></a>
    <div class="lightbox-caption">` + caption + `<div class="credit">Credit: Sam Lens</div></div>
  </div>
  <figcaption><div class="caption"><div class="icon"></div><div>` + caption +
		` <span class="credit">Credit: Sam Lens</span></div></div></figcaption>
</figure>
<p>The work is expected to finish in spring.</p>
</div>
</body></html>`

	got, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://news.example.com/2026/11/the-roof",
		Rule:    &extract.Rule{ContentSelector: ".post-content"},
	})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if n := strings.Count(got.HTML, "photographed in March"); n != 1 {
		t.Errorf("caption appears %d times in the body, want 1:\n%s", n, got.HTML)
	}
	if n := strings.Count(got.HTML, "Sam Lens"); n != 1 {
		t.Errorf("credit appears %d times in the body, want 1", n)
	}
	for _, want := range []string{"<figcaption>", `alt="A brick wall"`, `title="Taken from the car park"`} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("body is missing %s:\n%s", want, got.HTML)
		}
	}
}

// An element holding an image is never a caption's copy, even when its text matches.
func TestCaptionCopyCheckSparesImages(t *testing.T) {
	const caption = "A short caption"
	got := extract.PruneForTest(`<figure><div><img src="a.jpg" alt="x">` + caption +
		`</div><figcaption>` + caption + `</figcaption></figure>`)
	if !strings.Contains(got, "<img") || strings.Count(got, caption) != 2 {
		t.Errorf("an element holding an image was removed:\n%s", got)
	}
}

func TestPruneUnseen(t *testing.T) {
	tests := []struct {
		name, in      string
		want, notWant []string
	}{
		{
			name:    "inline display none",
			in:      `<p>seen</p><div style="color:red; DISPLAY : none !important">unseen</div>`,
			want:    []string{"seen"},
			notWant: []string{"unseen"},
		},
		{
			name:    "inline visibility hidden",
			in:      `<p>seen</p><span style="visibility:hidden">unseen</span>`,
			want:    []string{"seen"},
			notWant: []string{"unseen"},
		},
		{
			// Streaming server rendering sends the finished article this way and swaps
			// it in from script. Removing it emptied every post on one site.
			name: "hidden attribute is kept",
			in:   `<div hidden id="S:0"><p>the whole article</p></div>`,
			want: []string{"the whole article"},
		},
		{
			// Anti-flicker snippets hide the whole body and reveal it from script.
			name: "a hidden body is kept",
			in:   `<html><body style="display:none"><p>the whole article</p></body></html>`,
			want: []string{"the whole article"},
		},
		{
			// A hidden image goes, and its alt text with it: it is not on the page.
			name:    "hidden image",
			in:      `<p>seen</p><img src="b.jpg" alt="placeholder" style="display:none">`,
			notWant: []string{"placeholder"},
		},
		{
			name: "display other than none",
			in:   `<div style="display:flex">seen</div>`,
			want: []string{"seen"},
		},
		{
			name: "a figure's caption copy outside the figure is not touched",
			in:   `<div>Same words</div><figure><img src="a.jpg"><figcaption>Same words</figcaption></figure>`,
			want: []string{"<div>Same words</div>", "<figcaption>Same words</figcaption>"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extract.PruneForTest(tc.in)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in:\n%s", w, got)
				}
			}
		})
	}
}
