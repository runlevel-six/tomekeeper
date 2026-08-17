package server

import (
	"html/template"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// In-package, because templateFuncs is unexported and this is the one piece of
// template logic with a security property attached.

func snippetFunc(t *testing.T) func(string) template.HTML {
	t.Helper()

	fn, ok := templateFuncs["snippet"].(func(string) template.HTML)
	if !ok {
		t.Fatalf("the snippet template function has an unexpected type %T", templateFuncs["snippet"])
	}
	return fn
}

// The snippet is article text, and article text can be anything a writer typed.
// Escaping it and then reintroducing only <mark> is the whole defense; if it ever
// stops holding, a post about HTML becomes a stored cross-site scripting payload.
func TestSnippetEscaping(t *testing.T) {
	snippet := snippetFunc(t)

	hl := func(s string) string { return store.HighlightStart + s + store.HighlightEnd }

	cases := []struct {
		name       string
		in         string
		wantHave   []string
		wantAbsent []string
	}{
		{
			name:       "a script tag in the article text",
			in:         "Writing <script>alert(1)</script> about " + hl("markup"),
			wantHave:   []string{"&lt;script&gt;", "<mark>markup</mark>"},
			wantAbsent: []string{"<script>", "</script>"},
		},
		{
			name:       "an image with an event handler",
			in:         `An <img src=x onerror="alert(1)"> in ` + hl("prose"),
			wantHave:   []string{"&lt;img", "<mark>prose</mark>"},
			wantAbsent: []string{"<img"},
		},
		{
			name:       "a quote that would break out of an attribute",
			in:         `He said "><script>alert(1)</script> about ` + hl("quoting"),
			wantAbsent: []string{"<script>"},
			wantHave:   []string{"<mark>quoting</mark>"},
		},
		{
			name:     "ampersands are not double-escaped into nonsense",
			in:       "Smith & Sons on " + hl("trade"),
			wantHave: []string{"Smith &amp; Sons", "<mark>trade</mark>"},
		},
		{
			name: "a sentinel the article itself contains",
			// The failure mode here is a stray highlight, which is visibly odd and
			// harmless. What must not happen is markup.
			in:         "An article mentioning [[hl]] literally, about " + hl("syntax"),
			wantAbsent: []string{"<script"},
			wantHave:   []string{"<mark>"},
		},
		{
			name:     "no highlight at all",
			in:       "Just a passage with no matched terms.",
			wantHave: []string{"Just a passage"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(snippet(c.in))

			for _, want := range c.wantHave {
				if !strings.Contains(got, want) {
					t.Errorf("output is missing %q\ngot: %s", want, got)
				}
			}
			for _, absent := range c.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("output contains %q, which is live markup from article text\ngot: %s", absent, got)
				}
			}
		})
	}
}

// The only tags the function may ever produce are the highlight's.
func TestSnippetProducesOnlyMarkTags(t *testing.T) {
	snippet := snippetFunc(t)

	got := string(snippet("a <b>bold</b> <i>claim</i> and an <a href=x>anchor</a> about " +
		store.HighlightStart + "markup" + store.HighlightEnd))

	// Every "<" in the output must begin a <mark> or </mark>.
	for i := 0; i < len(got); i++ {
		if got[i] != '<' {
			continue
		}
		rest := got[i:]
		if !strings.HasPrefix(rest, "<mark>") && !strings.HasPrefix(rest, "</mark>") {
			t.Fatalf("found a tag other than the highlight at offset %d: %s", i, rest[:min(20, len(rest))])
		}
	}
	if !strings.Contains(got, "<mark>markup</mark>") {
		t.Errorf("the highlight is missing: %s", got)
	}
}

func TestReadingTimeAndSince(t *testing.T) {
	readingTime, ok := templateFuncs["readingTime"].(func(int) string)
	if !ok {
		t.Fatal("readingTime has an unexpected type")
	}

	if got := readingTime(0); got != "" {
		t.Errorf("readingTime(0) = %q, want empty so the template omits it", got)
	}
	// Anything with words in it rounds up to at least a minute: "0 minutes read"
	// would be silly.
	if got := readingTime(5); got != "1 minute read" {
		t.Errorf("readingTime(5) = %q, want %q", got, "1 minute read")
	}
	if got := readingTime(440); got != "2 minutes read" {
		t.Errorf("readingTime(440) = %q, want %q", got, "2 minutes read")
	}
}
