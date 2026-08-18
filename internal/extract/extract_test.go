package extract_test

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"github.com/runlevel-six/tomekeeper/internal/extract"
)

// corpusEnvVar names a directory of further corpus cases held outside the
// repository.
//
// The corpus worth having is ~30 pages from sites actually being read, and
// those are third-party copyrighted content, so they are not committed here —
// see the README in testdata/pages. Point this at a private checkout and the
// same golden test covers them; leave it unset and only the synthetic fixtures
// run, which is what a contributor gets.
//
// Unset means skip, matching internal/dbtest. A skipped case is not a passing
// case, so `task test:corpus` fails when this is missing, the same way
// `task test:integration` fails without a database.
const corpusEnvVar = "TOME_TEST_CORPUS_DIR"

// TestCorpus runs every case in testdata/pages, plus any in the private corpus.
//
// This is the regression suite for every extractor change, and the place a new
// site that extracts badly gets added before anything is fixed. See the README
// in that directory for the file format.
func TestCorpus(t *testing.T) {
	committed := committedCorpus(t)
	private := privateCorpus(t, committed)

	// Printed on every run, passing or not: the number is how you notice that
	// the private corpus quietly stopped being loaded.
	t.Logf("corpus: %d committed fixtures, %d private pages", len(committed), len(private))

	cases := make([]corpusCase, 0, len(committed)+len(private))
	cases = append(cases, committed...)
	cases = append(cases, private...)

	e := extract.New()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.Extract(tc.input)

			if tc.expectNone {
				if err == nil {
					t.Fatalf("Extract() produced %d characters, want no acceptable content:\n%s",
						len(got.Text), truncate(got.Text))
				}
				return
			}
			if err != nil {
				t.Fatalf("Extract() = %v, want content", err)
			}

			if tc.extractor != "" && got.Name != tc.extractor {
				t.Errorf("extractor = %q, want %q", got.Name, tc.extractor)
			}
			if tc.minChars > 0 && len(got.Text) < tc.minChars {
				t.Errorf("extracted %d characters, want at least %d:\n%s",
					len(got.Text), tc.minChars, truncate(got.Text))
			}

			// Asserted against the body as well as the text, and the reason is a
			// bug that hid here for a milestone. Result.Text and Result.HTML are
			// two renderings of one document and are stored in one row, but nothing
			// checked that they agreed — so a domain rule that emitted the first of
			// three content blocks while reporting the text of all three passed
			// every threshold and every substring assertion. The reader saw a third
			// of the article; search indexed the rest. Whatever the text must
			// contain, the body a reader is shown must contain too.
			// Compared with whitespace collapsed, so an assertion does not depend on
			// where the fixture's source happens to wrap a line. A phrase spanning a
			// newline in the saved page is the same phrase.
			text := normalizeSpace(got.Text)
			bodyText := normalizeSpace(textOfHTML(got.HTML))
			for _, want := range tc.contains {
				want = normalizeSpace(want)
				if !strings.Contains(text, want) {
					t.Errorf("extracted text is missing %q\n\ngot:\n%s", want, truncate(got.Text))
				}
				if !strings.Contains(bodyText, want) {
					t.Errorf("the stored body is missing %q, though the extracted text has it — "+
						"the body and the text have diverged\n\nbody:\n%s", want, truncate(bodyText))
				}
			}
			if tc.minImages > 0 {
				if n := countImages(got.HTML); n < tc.minImages {
					t.Errorf("the stored body holds %d images, want at least %d:\n%s",
						n, tc.minImages, truncate(got.HTML))
				}
			}
			if n := duplicateImages(got.HTML); n > 0 {
				t.Errorf("the stored body repeats %d picture(s). Two things cause this: a "+
					"selector matching an element inside another match, and a site shipping "+
					"several sizes of one image for different screens with the extras hidden "+
					"by CSS that this archive does not keep:\n%s", n, truncate(got.HTML))
			}
			// The assertions that catch the regression worth catching: an
			// extractor that starts including navigation or cookie banners
			// still looks fine by length alone.
			for _, unwanted := range tc.excludes {
				if strings.Contains(text, normalizeSpace(unwanted)) {
					t.Errorf("extracted text contains %q, which is page chrome, not article\n\ngot:\n%s",
						unwanted, truncate(got.Text))
				}
			}

			if got.WordCount == 0 {
				t.Error("WordCount = 0 for a non-empty body")
			}
		})
	}
}

// Sanitization is a security property, so it gets its own assertions rather
// than relying on the corpus's text-level checks.
func TestSanitizationRemovesActiveContent(t *testing.T) {
	tc := findCase(t, "hostile-markup")

	got, err := extract.New().Extract(tc.input)
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}

	forbidden := []struct {
		fragment string
		why      string
	}{
		{"<script", "a script tag would run in the reader's browser on the reader's origin"},
		{"onclick", "an inline event handler is a script by another name"},
		{"javascript:", "a javascript: URL executes when clicked"},
		{"<iframe", "an iframe reaches back out to a third party on every read"},
		{"evil.example.com", "the exfiltration target survived"},
		{"tracker.example.com", "the tracking pixel survived"},
	}
	for _, f := range forbidden {
		if strings.Contains(strings.ToLower(got.HTML), f.fragment) {
			t.Errorf("sanitized body contains %q: %s\n\n%s", f.fragment, f.why, got.HTML)
		}
	}

	// Removing the dangerous parts must not cost the article.
	if !strings.Contains(got.HTML, "ordinary") {
		t.Errorf("sanitization damaged the article body:\n%s", got.HTML)
	}
}

// The asset pipeline works from the stored body rather than the live page, so
// every reference in it has to be absolute by the time it is stored.
func TestRelativeURLsAreResolved(t *testing.T) {
	tc := findCase(t, "hostile-markup")

	got, err := extract.New().Extract(tc.input)
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}

	// The article is at /posts/2026/ordinary, so "../images/photo.jpg"
	// resolves to /posts/images/photo.jpg — relative to the article's
	// directory, not to the site root.
	for _, want := range []string{
		"https://example.com/relative/page",
		"https://example.com/posts/images/photo.jpg",
	} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("body does not contain the resolved reference %q\n\n%s", want, got.HTML)
		}
	}

	// Every candidate in a srcset must be resolved, not just the first.
	for _, want := range []string{
		"https://example.com/posts/images/photo-480.jpg 480w",
		"https://example.com/posts/images/photo-960.jpg 960w",
	} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("srcset candidate %q was not resolved\n\n%s", want, got.HTML)
		}
	}

	if strings.Contains(got.HTML, "../images/") {
		t.Errorf("a relative reference survived into the stored body:\n%s", got.HTML)
	}
}

// A rule is a human saying "the body is here", so it wins over the heuristics.
func TestDomainRuleTakesPrecedence(t *testing.T) {
	tc := findCase(t, "needs-domain-rule")

	withRule, err := extract.New().Extract(tc.input)
	if err != nil {
		t.Fatalf("Extract() with a rule = %v", err)
	}
	if withRule.Name != extract.NameDomainRule {
		t.Errorf("extractor = %q, want %q", withRule.Name, extract.NameDomainRule)
	}

	// Without the rule, this page is exactly the kind the heuristics get
	// wrong — which is why the fixture exists.
	noRule := tc.input
	noRule.Rule = nil

	if got, err := extract.New().Extract(noRule); err == nil {
		if strings.Contains(got.Text, "Subscribe today") {
			t.Log("without the rule, extraction picks up the promotional block — this is the case the rule exists for")
		}
	}
}

// A rule that no longer matches means the site was redesigned. Falling through
// to the heuristics beats returning nothing.
func TestStaleDomainRuleFallsThrough(t *testing.T) {
	tc := findCase(t, "semantic-article")
	in := tc.input
	in.Rule = &extract.Rule{ContentSelector: "div.this-selector-matches-nothing"}

	got, err := extract.New().Extract(in)
	if err != nil {
		t.Fatalf("Extract() = %v, want a fallback to the heuristics", err)
	}
	if got.Name == extract.NameDomainRule {
		t.Error("a rule that matched nothing was reported as the extractor")
	}
	if !strings.Contains(got.Text, "link that worked in 2011") {
		t.Errorf("the fallback did not recover the article:\n%s", truncate(got.Text))
	}
}

// The feed body is the last rung, used only when there is no page to work from.
func TestFeedBodyIsTheLastResort(t *testing.T) {
	tc := findCase(t, "semantic-article")

	// With a page, the page wins even though a feed body is available.
	in := tc.input
	in.FeedBody = "<p>" + strings.Repeat("A feed summary. ", 40) + "</p>"

	got, err := extract.New().Extract(in)
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if got.Name == extract.NameFeedBody {
		t.Error("the feed body was used even though the page extracted successfully")
	}

	// With no page, it is used.
	in.RawHTML = nil
	got, err = extract.New().Extract(in)
	if err != nil {
		t.Fatalf("Extract() with no page = %v", err)
	}
	if got.Name != extract.NameFeedBody {
		t.Errorf("extractor = %q, want %q", got.Name, extract.NameFeedBody)
	}
}

// A truncated feed summary is not an article. Storing it as one would make the
// archive look complete when it is not.
func TestShortFeedBodyIsRejected(t *testing.T) {
	got, err := extract.New().Extract(extract.Input{
		URL:      "https://example.com/x",
		FeedBody: "<p>Read the rest of this entry.</p>",
	})
	if err == nil {
		t.Errorf("Extract() accepted a %d character summary as an article: %q", len(got.Text), got.Text)
	}
}

func TestNothingToExtract(t *testing.T) {
	if _, err := extract.New().Extract(extract.Input{URL: "https://example.com/x"}); err == nil {
		t.Error("Extract() with no page and no feed body = nil, want an error")
	}
}

func TestInvalidURL(t *testing.T) {
	if _, err := extract.New().Extract(extract.Input{URL: "://not a url"}); err == nil {
		t.Error("Extract() with an unparseable URL = nil, want an error")
	}
}

// --- corpus loading ---

type corpusCase struct {
	name       string
	input      extract.Input
	extractor  string
	minChars   int
	minImages  int
	contains   []string
	excludes   []string
	expectNone bool
}

// committedCorpus loads the synthetic fixtures that ship with the repository.
//
// The named-fixture tests below use only these: they assert behavior against
// structures built to exercise one rung each, which is a different job from the
// breadth the private corpus provides.
func committedCorpus(t *testing.T) []corpusCase {
	t.Helper()

	cases := loadCorpusDir(t, filepath.Join("testdata", "pages"))
	if len(cases) == 0 {
		t.Fatal("the committed corpus is empty")
	}
	return cases
}

// privateCorpus loads the real pages named by corpusEnvVar, or nothing when it
// is unset.
//
// Every failure here is fatal rather than a fall back to the synthetic set. A
// variable that is set but unusable would otherwise drop the entire real corpus
// while the suite still reported green, which is precisely the silent loss this
// mechanism exists to prevent.
func privateCorpus(t *testing.T, committed []corpusCase) []corpusCase {
	t.Helper()

	dir := os.Getenv(corpusEnvVar)
	if dir == "" {
		return nil
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("%s=%q: %v", corpusEnvVar, dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s=%q is not a directory", corpusEnvVar, dir)
	}

	cases := loadCorpusDir(t, dir)
	if len(cases) == 0 {
		t.Fatalf("%s=%q holds no *.want files; a corpus that loads nothing is not a corpus", corpusEnvVar, dir)
	}

	byName := make(map[string]bool, len(committed))
	for _, tc := range committed {
		byName[tc.name] = true
	}
	for _, tc := range cases {
		// Duplicate stems would mean one case silently shadows the other in
		// the test output, decided by glob order.
		if byName[tc.name] {
			t.Fatalf("corpus case %q is in both testdata/pages and %s; it belongs in one or the other", tc.name, dir)
		}
	}
	return cases
}

func loadCorpusDir(t *testing.T, dir string) []corpusCase {
	t.Helper()

	wants, err := filepath.Glob(filepath.Join(dir, "*.want"))
	if err != nil {
		t.Fatalf("globbing corpus in %s: %v", dir, err)
	}

	cases := make([]corpusCase, 0, len(wants))
	for _, wantPath := range wants {
		name := strings.TrimSuffix(filepath.Base(wantPath), ".want")
		cases = append(cases, loadCase(t, dir, name, wantPath))
	}
	return cases
}

func loadCase(t *testing.T, dir, name, wantPath string) corpusCase {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, name+".html"))
	if err != nil {
		t.Fatalf("%s: reading page: %v", name, err)
	}

	f, err := os.Open(wantPath)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	defer func() { _ = f.Close() }()

	tc := corpusCase{name: name}
	tc.input.RawHTML = raw

	var rule extract.Rule
	inHeaders := true

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()

		if inHeaders {
			if strings.TrimSpace(line) == "" {
				inHeaders = false
				continue
			}
			// Comments are allowed among the headers as well as below them, so a
			// header can carry the reason it exists next to the header itself.
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				t.Fatalf("%s: header line without a colon: %q", name, line)
			}
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)

			switch key {
			case "url":
				tc.input.URL = value
			case "extractor":
				tc.extractor = value
			case "min_chars":
				n, err := strconv.Atoi(value)
				if err != nil {
					t.Fatalf("%s: min_chars %q: %v", name, value, err)
				}
				tc.minChars = n
			case "min_images":
				n, err := strconv.Atoi(value)
				if err != nil {
					t.Fatalf("%s: min_images %q: %v", name, value, err)
				}
				tc.minImages = n
			case "selector":
				rule.ContentSelector = value
			case "strip":
				rule.StripSelectors = append(rule.StripSelectors, value)
			case "feed_body":
				body, err := os.ReadFile(filepath.Join(dir, value))
				if err != nil {
					t.Fatalf("%s: reading feed body: %v", name, err)
				}
				tc.input.FeedBody = string(body)
			case "expect":
				tc.expectNone = value == "none"
			default:
				t.Fatalf("%s: unknown header %q", name, key)
			}
			continue
		}

		trimmed := strings.TrimSpace(line)
		// A comment. `tome corpus add` writes the article's opening and closing
		// sentences into a captured case as comments, so that choosing a phrase to
		// assert on means reading rather than hunting through a browser tab.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed != "" {
			if after, found := strings.CutPrefix(trimmed, "!"); found {
				tc.excludes = append(tc.excludes, strings.TrimSpace(after))
				continue
			}
			tc.contains = append(tc.contains, trimmed)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	if tc.input.URL == "" {
		t.Fatalf("%s: the want file has no url header", name)
	}
	if rule.ContentSelector != "" || len(rule.StripSelectors) > 0 {
		tc.input.Rule = &rule
	}
	return tc
}

func findCase(t *testing.T, name string) corpusCase {
	t.Helper()
	for _, tc := range committedCorpus(t) {
		if tc.name == name {
			return tc
		}
	}
	t.Fatalf("committed corpus case %q not found", name)
	return corpusCase{}
}

func truncate(s string) string {
	const limit = 600
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// imgSrcPattern finds the source of every image in a stored body.
var imgSrcPattern = regexp.MustCompile(`(?i)<img[^>]+src="([^"]*)"`)

func countImages(body string) int {
	return len(imgSrcPattern.FindAllString(body, -1))
}

// sizeVariant matches the width×height suffix a CDN appends to a resized image:
// `photo-640x427.jpg` is `photo.jpg` at another size.
var sizeVariant = regexp.MustCompile(`-\d+x\d+(\.\w+)$`)

// samePicture reduces an image URL to the picture it shows.
//
// Two sizes of one photograph are one picture to a reader, and telling them apart
// is what let a real duplicate reach production: a site shipped a 640px and a
// 1152px copy of its lead image in the same link, hiding one with a CSS class that
// means nothing once the stylesheet is gone. Both rendered, the exact sources
// differed, and a check comparing them byte for byte said everything was fine.
func samePicture(src string) string {
	if i := strings.Index(src, "?"); i >= 0 {
		src = src[:i]
	}
	if i := strings.LastIndex(src, "/"); i >= 0 {
		src = src[i+1:]
	}
	return sizeVariant.ReplaceAllString(src, "$1")
}

// duplicateImages counts pictures that appear more than once.
//
// A body is a document, and a document does not show the same picture twice in a
// row. Two ways that happens: a selector list naming an element and something
// inside it, and a site serving several sizes of one image for different screens.
func duplicateImages(body string) int {
	seen := map[string]int{}
	for _, m := range imgSrcPattern.FindAllStringSubmatch(body, -1) {
		seen[samePicture(m[1])]++
	}
	repeats := 0
	for _, n := range seen {
		if n > 1 {
			repeats++
		}
	}
	return repeats
}

// textOfHTML is everything a stored body says, for asserting that the body and
// the extracted text agree.
//
// Alt attributes count as what the body says. The image rung's text is the alt
// text of the pictures it selected — a comic's body is a picture and its title —
// so reading only the visible text would report that rung as diverged from itself
// on every case, which is a false alarm rather than a finding.
func textOfHTML(body string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return ""
	}

	var alts strings.Builder
	doc.Find("img[alt]").Each(func(_ int, img *goquery.Selection) {
		alts.WriteString(" " + img.AttrOr("alt", ""))
	})
	return doc.Text() + alts.String()
}

// normalizeSpace collapses every run of whitespace to one space.
//
// Fixtures are saved pages, and a saved page wraps its prose wherever the site's
// templating happened to wrap it. Asserting on exact whitespace would mean every
// assertion silently depends on that, and a phrase that reads as one sentence would
// fail for being written across two lines.
func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// An inline image survives extraction; anything else wearing a data: URI does not.
//
// Three other parts of the archive assume inline images are kept — resolveURLs steps
// over them, the asset policy has a skip reason for them, and the reader's content
// security policy allows `img-src data:` for them. None of that was reachable while
// the sanitizer's scheme allowlist stripped every data: URI first, so the allowance
// was dead code and every inline image was quietly dropped.
//
// The narrow part matters as much as the allowance: `data:image/svg+xml` is a
// document that can carry script, and `data:text/html` is a page. Neither is an
// image for this purpose, whatever its media type claims.
func TestInlineImagesSurviveButOtherDataURIsDoNot(t *testing.T) {
	tc := findCase(t, "hostile-markup")

	got, err := extract.New().Extract(tc.input)
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}

	if !strings.Contains(got.HTML, "data:image/gif;base64,R0lGODlhAQABAAAAACH5BAEKAAEALAAAAAABAAEAAAICTAEAOw==") {
		t.Errorf("the inline image was stripped:\n%s", got.HTML)
	}

	for _, forbidden := range []struct{ fragment, why string }{
		{"data:text/html", "a data: URI carrying a page is not an image"},
		{"data:image/svg+xml", "an SVG is a document and can carry script"},
	} {
		if strings.Contains(got.HTML, forbidden.fragment) {
			t.Errorf("%s survived: %s\n\n%s", forbidden.fragment, forbidden.why, got.HTML)
		}
	}
}

// Text has a boundary where the markup has one.
//
// goquery's Text() welds text nodes together, so `<p>service.</p><h2>Data center</h2>`
// became "service.Data center" — measured at 16 of 341 bodies in a real archive. The
// length checks measure that text, the search index tokenizes it, and an excerpt
// shows it to a reader.
func TestTextHasBoundariesAtBlockEdges(t *testing.T) {
	body := `<div><p>Ubuntu on real servers turns your data center into a bare metal cloud.` +
		`Welcome to metal-as-a-service.</p><h2>Data center automation</h2>` +
		`<ul><li>Provisioning</li><li>Deployment</li></ul>` +
		`<table><tr><td>Rack</td><td>Ready</td></tr></table>` +
		`<p>One sentence<br>split across a line break.</p></div>`

	got := extract.TextForTest([]byte(body))
	collapsed := strings.Join(strings.Fields(got), " ")

	for _, want := range []string{
		"metal-as-a-service. Data center automation",
		"Provisioning Deployment",
		"Rack Ready",
		"One sentence split across a line break.",
	} {
		if !strings.Contains(collapsed, want) {
			t.Errorf("no boundary where the markup has one: want %q in\n%s", want, collapsed)
		}
	}

	// And no boundary where the markup has none: inline elements do not separate
	// words, or every emphasized word in the archive would split in two.
	inline := extract.TextForTest([]byte(`<p>a <em>single</em> word: un<strong>break</strong>able</p>`))
	if collapsed := strings.Join(strings.Fields(inline), " "); collapsed != "a single word: unbreakable" {
		t.Errorf("inline markup introduced a boundary: %q", collapsed)
	}
}

// The duplicate-image check counts pictures rather than URLs.
//
// It did not, and that is how a repeated lead image reached a reader: a site shipped
// two sizes of one photograph in the same link and hid one with a CSS class, which
// means nothing in an archive that keeps the HTML and drops the stylesheet. The
// sources differed by a size suffix, so a byte-for-byte comparison reported no
// duplicate at all.
func TestDuplicateImagesCountsPicturesNotURLs(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{
			name: "the same URL twice",
			body: `<img src="https://cdn.example.com/photo.jpg"><img src="https://cdn.example.com/photo.jpg">`,
			want: 1,
		},
		{
			name: "two sizes of one picture",
			body: `<img src="https://cdn.example.com/photo-640x427.jpg">` +
				`<img src="https://cdn.example.com/photo-1152x648.jpg">`,
			want: 1,
		},
		{
			name: "a size variant and the original",
			body: `<img src="https://cdn.example.com/photo-640x427.jpg">` +
				`<img src="https://cdn.example.com/photo.jpg">`,
			want: 1,
		},
		{
			name: "genuinely different pictures",
			body: `<img src="https://cdn.example.com/first-640x427.jpg">` +
				`<img src="https://cdn.example.com/second-640x427.jpg">`,
			want: 0,
		},
		{
			// Two files whose names differ by more than a size are two pictures,
			// even when one looks like a variant of the other.
			name: "similar names, different pictures",
			body: `<img src="https://cdn.example.com/ship40_1.jpg">` +
				`<img src="https://cdn.example.com/ship40_2.jpg">`,
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := duplicateImages(tc.body); got != tc.want {
				t.Errorf("duplicateImages() = %d, want %d", got, tc.want)
			}
		})
	}
}
