package urlcanon_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/urlcanon"
)

// TestCanonicalizeGolden runs the corpus in testdata/urls/canonical.txt.
//
// The corpus is the specification. A deduplication bug is fixed by adding a
// line there first, watching it fail, and then changing the code.
func TestCanonicalizeGolden(t *testing.T) {
	for _, tc := range readGolden(t, "canonical.txt") {
		t.Run(tc.input, func(t *testing.T) {
			got, err := urlcanon.Canonicalize(tc.input)
			if err != nil {
				t.Fatalf("Canonicalize(%q) = error %v, want %q", tc.input, err, tc.want)
			}
			if got != tc.want {
				t.Errorf("Canonicalize(%q)\n got: %q\nwant: %q", tc.input, got, tc.want)
			}
		})
	}
}

// Canonicalization must be idempotent: feeding a canonical URL back through
// must not change it. Without this, an article re-seen through a different
// path could produce a second row on the second pass.
func TestCanonicalizeIsIdempotent(t *testing.T) {
	for _, tc := range readGolden(t, "canonical.txt") {
		t.Run(tc.input, func(t *testing.T) {
			once, err := urlcanon.Canonicalize(tc.input)
			if err != nil {
				t.Fatalf("Canonicalize(%q) = %v", tc.input, err)
			}
			twice, err := urlcanon.Canonicalize(once)
			if err != nil {
				t.Fatalf("Canonicalize(%q) = %v on the second pass", once, err)
			}
			if once != twice {
				t.Errorf("not idempotent:\nfirst:  %q\nsecond: %q", once, twice)
			}
		})
	}
}

func TestCanonicalizeRejectsInvalid(t *testing.T) {
	for _, input := range readLines(t, "invalid.txt") {
		t.Run(input, func(t *testing.T) {
			got, err := urlcanon.Canonicalize(input)
			if err == nil {
				t.Errorf("Canonicalize(%q) = %q, want an error", input, got)
			}
		})
	}
}

func TestCanonicalizeRejectsEmpty(t *testing.T) {
	for _, input := range []string{"", " ", "\t", "\n  \n"} {
		if got, err := urlcanon.Canonicalize(input); err == nil {
			t.Errorf("Canonicalize(%q) = %q, want an error", input, got)
		}
	}
}

// The property that makes an article the root entity: the same story arriving
// through several feeds, each decorating the link differently, is one article.
func TestSyndicatedVariantsCollapse(t *testing.T) {
	variants := []string{
		"https://example.com/2026/08/the-story",
		"https://example.com/2026/08/the-story/",
		"https://example.com/2026/08/the-story?utm_source=feedly&utm_medium=rss",
		"https://EXAMPLE.com/2026/08/the-story#comments",
		"https://example.com:443/2026/08/the-story?fbclid=IwAR0abc",
		"  https://example.com/2026/08/the-story/?ref=hn  ",
	}

	want, err := urlcanon.Canonicalize(variants[0])
	if err != nil {
		t.Fatalf("Canonicalize(%q) = %v", variants[0], err)
	}

	for _, v := range variants[1:] {
		got, err := urlcanon.Canonicalize(v)
		if err != nil {
			t.Fatalf("Canonicalize(%q) = %v", v, err)
		}
		if got != want {
			t.Errorf("Canonicalize(%q) = %q, want %q — these are the same article", v, got, want)
		}
	}
}

// The opposite direction, which is the expensive one to get wrong: distinct
// articles must stay distinct.
func TestDistinctArticlesStayDistinct(t *testing.T) {
	distinct := []string{
		"https://example.com/?p=1",
		"https://example.com/?p=2",
		"https://example.com/article",
		"https://example.com/Article",
		"https://example.com/article?page=2",
		"http://example.com/article",
		"https://other.example.com/article",
		"https://example.com:8080/article",
	}

	seen := make(map[string]string, len(distinct))
	for _, raw := range distinct {
		got, err := urlcanon.Canonicalize(raw)
		if err != nil {
			t.Fatalf("Canonicalize(%q) = %v", raw, err)
		}
		if prev, collision := seen[got]; collision {
			t.Errorf("%q and %q both canonicalize to %q, merging two articles", prev, raw, got)
		}
		seen[got] = raw
	}
}

func TestIsTrackingParam(t *testing.T) {
	tracking := []string{"utm_source", "UTM_SOURCE", "fbclid", "gclid", "ref", "utm_anything_at_all"}
	for _, p := range tracking {
		if !urlcanon.IsTrackingParam(p) {
			t.Errorf("IsTrackingParam(%q) = false, want true", p)
		}
	}

	meaningful := []string{"p", "id", "story_id", "page", "q", "utm", "reference"}
	for _, p := range meaningful {
		if urlcanon.IsTrackingParam(p) {
			t.Errorf("IsTrackingParam(%q) = true, want false", p)
		}
	}
}

func FuzzCanonicalize(f *testing.F) {
	for _, tc := range readGolden(f, "canonical.txt") {
		f.Add(tc.input)
	}

	// Canonicalize must never panic, and whatever it accepts it must accept
	// again unchanged. Feed URLs are attacker-adjacent input: anyone whose
	// feed this archive subscribes to chooses these strings.
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := urlcanon.Canonicalize(raw)
		if err != nil {
			return
		}
		again, err := urlcanon.Canonicalize(got)
		if err != nil {
			t.Fatalf("Canonicalize(%q) = %q, which then failed: %v", raw, got, err)
		}
		if got != again {
			t.Errorf("not idempotent for %q:\nfirst:  %q\nsecond: %q", raw, got, again)
		}
	})
}

type goldenCase struct {
	input string
	want  string
}

func readGolden(tb testing.TB, name string) []goldenCase {
	tb.Helper()

	var cases []goldenCase
	for i, line := range readLines(tb, name) {
		input, want, ok := strings.Cut(line, "=>")
		if !ok {
			tb.Fatalf("%s line %d: no => separator: %q", name, i+1, line)
		}
		cases = append(cases, goldenCase{
			input: strings.TrimSpace(input),
			want:  strings.TrimSpace(want),
		})
	}
	if len(cases) == 0 {
		tb.Fatalf("%s contains no cases", name)
	}
	return cases
}

func readLines(tb testing.TB, name string) []string {
	tb.Helper()

	path := filepath.Join("testdata", "urls", name)
	f, err := os.Open(path)
	if err != nil {
		tb.Fatalf("opening corpus: %v", err)
	}
	defer func() { _ = f.Close() }()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		tb.Fatalf("reading corpus: %v", err)
	}
	return lines
}
