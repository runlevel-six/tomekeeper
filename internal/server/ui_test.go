package server

import (
	"html/template"
	"io/fs"
	"path"
	"regexp"
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

// A template that fails to parse does not fail loudly: New logs the error and
// mounts the health endpoints only, so the whole web interface becomes 404s. The
// symptom is every handler test failing to sign in, which reads like a broken
// session rather than a broken template.
//
// This is the test that names the real problem. It exists because `{{else with}}`
// — which is not valid Go template syntax, though `{{else if}} is — took the
// entire UI down and the failure it produced pointed somewhere else entirely.
func TestEveryPageTemplateParses(t *testing.T) {
	if _, err := newUI(); err != nil {
		t.Fatalf("newUI() = %v\n\nthe whole web interface would serve 404s with this error", err)
	}
}

// Every page listed must exist, and every page that exists must be listed. A page
// template added without being listed is silently never served.
func TestPageNamesMatchTheTemplateFiles(t *testing.T) {
	files, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		t.Fatalf("globbing the templates: %v", err)
	}

	// base and partials are included by every page rather than being pages.
	shared := map[string]bool{"base": true, "partials": true, "mark": true}

	onDisk := make(map[string]bool)
	for _, f := range files {
		name := strings.TrimSuffix(path.Base(f), ".html")
		if !shared[name] {
			onDisk[name] = true
		}
	}

	listed := make(map[string]bool, len(pageNames))
	for _, name := range pageNames {
		listed[name] = true
		if !onDisk[name] {
			t.Errorf("pageNames lists %q but templates/%s.html does not exist", name, name)
		}
	}
	for name := range onDisk {
		if !listed[name] {
			t.Errorf("templates/%s.html exists but is not in pageNames, so it is never served", name)
		}
	}
}

// Every class a template uses is either styled or listed here as deliberately not.
//
// This test exists because four pages shipped unstyled and nothing noticed. `.page`
// was written into the markup of Accounts, the explanation, the audit, the reprocess
// page and the set-a-password page, and there was no rule behind it — so they
// rendered full-width with body-font headings and no separation between sections,
// while Settings, which spelled the same intentions out under its own class, looked
// like the rest of the application. The explanation's ladder table was worse: `.won`
// marks the rung that produced the stored body and drew nothing at all.
//
// A class with no rule is not a bug by itself. Section names are hooks that exist to
// say what a section *is*, and they inherit their appearance from `.page`. So the
// allowlist below is the point of this test: it forces the question "did I mean to
// leave this unstyled" to be answered once, in writing, rather than discovered by
// somebody looking at a page that seems unfinished.
func TestEveryTemplateClassIsStyledOrExempt(t *testing.T) {
	// Section and page names. Each of these labels a block whose appearance comes
	// from `.page` or from the element it wraps; a rule of its own would be one more
	// place for the same look to drift.
	exempt := map[string]string{
		"audit":           "page name; framed by .page",
		"explain":         "page name; framed by .page",
		"reprocess":       "page name; framed by .page",
		"setpassword":     "page name; framed by .page",
		"users":           "page name; framed by .page",
		"add":             "section name on Accounts",
		"backup":          "section name on Accounts, framed by .page",
		"confirm":         "section name on Accounts",
		"issued":          "section name on Accounts",
		"explain-ladder":  "section name on the explanation",
		"explain-rule":    "section name on the explanation",
		"explain-stored":  "section name on the explanation",
		"explain-subject": "section name on the explanation",
		"extractions":     "section name in Settings",
		"leaving":         "section name in Settings",
		"palettes":        "section name in Settings",
		"password":        "section name in Settings",
		"retention":       "section name in Settings",
		"sessions":        "section name in Settings",
		"username":        "section name in Settings",
		// Inline metadata that inherits the type around it on purpose: a byline and a
		// reading time are part of the same line as the date beside them.
		"byline":       "inline article metadata",
		"length":       "inline article metadata",
		"tags":         "inline article metadata",
		"signed-in-as": "inline chrome text",
		"signout":      "a form that is one button in the chrome",
		"secondary":    "a navigation link that takes the nav's own styling",
		"starred":      "names which search filter is in force; no appearance of its own",
	}

	css, err := assets.ReadFile("static/tome.css")
	if err != nil {
		t.Fatalf("reading the stylesheet: %v", err)
	}
	styled := make(map[string]bool)
	for _, m := range regexp.MustCompile(`\.([A-Za-z][A-Za-z0-9_-]*)`).FindAllStringSubmatch(string(css), -1) {
		styled[m[1]] = true
	}

	files, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		t.Fatalf("globbing the templates: %v", err)
	}

	classAttr := regexp.MustCompile(`class="([^"{}]+)"`)
	for _, f := range files {
		body, err := assets.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		for _, m := range classAttr.FindAllStringSubmatch(string(body), -1) {
			for _, class := range strings.Fields(m[1]) {
				if styled[class] || exempt[class] != "" {
					continue
				}
				t.Errorf("%s uses class %q with no rule in tome.css and no reason listed:\n"+
					"    style it, or add it to the allowlist in this test saying why it needs none",
					path.Base(f), class)
			}
		}
	}

	// And the allowlist itself has to stay honest: an entry for a class no template
	// uses any more is a claim about markup that is gone.
	used := make(map[string]bool)
	for _, f := range files {
		body, _ := assets.ReadFile(f)
		for _, m := range classAttr.FindAllStringSubmatch(string(body), -1) {
			for _, class := range strings.Fields(m[1]) {
				used[class] = true
			}
		}
	}
	for class := range exempt {
		if !used[class] {
			t.Errorf("the allowlist exempts %q, which no template uses; remove the entry", class)
		}
	}
}
