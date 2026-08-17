package extract_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/extract"
)

// TestCorpus runs every case in testdata/pages.
//
// This is the regression suite for every extractor change, and the place a new
// site that extracts badly gets added before anything is fixed. See the README
// in that directory for the file format.
func TestCorpus(t *testing.T) {
	cases := loadCorpus(t)
	if len(cases) == 0 {
		t.Fatal("the corpus is empty")
	}

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

			for _, want := range tc.contains {
				if !strings.Contains(got.Text, want) {
					t.Errorf("extracted text is missing %q\n\ngot:\n%s", want, truncate(got.Text))
				}
			}
			// The assertions that catch the regression worth catching: an
			// extractor that starts including navigation or cookie banners
			// still looks fine by length alone.
			for _, unwanted := range tc.excludes {
				if strings.Contains(got.Text, unwanted) {
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

// M3's asset pipeline works from the stored body rather than the live page, so
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
	contains   []string
	excludes   []string
	expectNone bool
}

func loadCorpus(t *testing.T) []corpusCase {
	t.Helper()

	dir := filepath.Join("testdata", "pages")
	wants, err := filepath.Glob(filepath.Join(dir, "*.want"))
	if err != nil {
		t.Fatalf("globbing corpus: %v", err)
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

		if trimmed := strings.TrimSpace(line); trimmed != "" {
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
	for _, tc := range loadCorpus(t) {
		if tc.name == name {
			return tc
		}
	}
	t.Fatalf("corpus case %q not found", name)
	return corpusCase{}
}

func truncate(s string) string {
	const limit = 600
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
