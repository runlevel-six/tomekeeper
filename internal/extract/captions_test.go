package extract_test

import (
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/extract"
)

func TestMarkCaptions(t *testing.T) {
	tests := []struct {
		name, in      string
		want, notWant []string
	}{
		{
			name: "a class naming a caption beside an image",
			in:   `<div class="wp-caption"><img src="a.jpg"><p class="wp-caption-text">The north wall.</p></div>`,
			want: []string{`<figcaption class="wp-caption-text">The north wall.</figcaption>`},
		},
		{
			name: "an image naming its label",
			in:   `<div><img src="a.jpg" aria-labelledby="cap-1"></div><div><div id="cap-1">The north wall.</div></div>`,
			want: []string{`<figcaption id="cap-1">The north wall.</figcaption>`},
		},
		{
			name:    "a wrapper holding the picture is not the caption",
			in:      `<div class="caption-wrap"><img src="a.jpg"><span>The north wall.</span></div>`,
			notWant: []string{"<figcaption"},
		},
		{
			// "captioned" and "recaptcha" contain the letters, not the word.
			name:    "a class that only contains the letters",
			in:      `<div><img src="a.jpg"><div class="captioned recaptcha">Not a caption.</div></div>`,
			notWant: []string{"<figcaption"},
		},
		{
			name:    "no image nearby",
			in:      `<div class="caption">A pull quote styled by its class.</div><p>Prose.</p>`,
			notWant: []string{"<figcaption"},
		},
		{
			// A figcaption in running text would end the paragraph where it stands.
			name:    "a caption-classed span inside a paragraph",
			in:      `<p><img src="a.jpg"> <span class="caption">inline</span> and more text.</p>`,
			notWant: []string{"<figcaption"},
		},
		{
			// An svg is as often a caption's own icon as a picture.
			name: "a caption carrying an icon",
			in:   `<div><img src="a.jpg"><div class="gallery-caption"><svg></svg>The north wall.</div></div>`,
			want: []string{`<figcaption class="gallery-caption">`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extract.MarkCaptionsForTest(tc.in)
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

// A domain rule stores the markup it selected, and a caption the site marked only with a
// class must still be stored as a caption: the sanitizer drops every class, and without
// the figcaption the reader shows the caption at body size.
func TestDomainRuleStoresClassCaptionsAsFigcaptions(t *testing.T) {
	page := `<!DOCTYPE html><html><head><title>The Roof</title></head><body>
<div class="post">` + articleParagraphs() + `
<div class="gallery">
  <div class="item"><img src="https://news.example.com/img/wall.jpg" alt="A brick wall" aria-labelledby="cap-7">
    <div class="lightbox-caption" id="cap-7">The north wall in March.</div></div>
</div>
</div></body></html>`

	got, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://news.example.com/2026/11/the-roof",
		Rule:    &extract.Rule{ContentSelector: ".post"},
	})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if !strings.Contains(got.HTML, "The north wall in March.</figcaption>") {
		t.Errorf("caption was not stored as a figcaption:\n%s", got.HTML)
	}
}
