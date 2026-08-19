package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/extract"
)

// A corpus file is named after the site and the article, not one or the other.
//
// Thirty pages from a dozen sites are much easier to read as a directory listing
// when the host is in the name, and two sites can easily publish the same slug.
func TestCorpusStem(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want string
	}{
		{"https://arstechnica.com/culture/2026/08/a-slasher-fans-dream/", "arstechnica-a-slasher-fans-dream"},
		{"https://www.example.co.uk/blog/posts/why-it-matters", "example-why-it-matters"},
		{"https://comics.example.com/comics/oots1347.html", "comics-oots1347"},
		{"https://example.com/Mixed_Case/Article!Name", "example-article-name"},
		// A URL with no path at all still yields something usable.
		{"https://example.com/", "example"},
		{"not a url", ""},
	} {
		t.Run(tc.url, func(t *testing.T) {
			if got := corpusStem(tc.url); got != tc.want {
				t.Errorf("corpusStem(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}

	// Long slugs are cut rather than producing a filename nothing can handle, and
	// never cut to a trailing dash.
	long := corpusStem("https://example.com/" + strings.Repeat("word-", 40))
	if len(long) > 60 {
		t.Errorf("a long slug produced a %d character stem", len(long))
	}
	if strings.HasSuffix(long, "-") {
		t.Errorf("stem ends in a dash: %q", long)
	}
}

// The starter file records what happened and leaves the judgment to a person.
func TestStarterWant(t *testing.T) {
	result := extract.Result{
		Name:      "trafilatura",
		HTML:      `<p>A body.</p><img src="https://example.com/one.png"><img src="https://example.com/two.png">`,
		Text:      strings.Repeat("word ", 200),
		WordCount: 200,
	}

	got := starterWant("https://example.com/posts/one", result, nil)

	for _, want := range []string{
		"url: https://example.com/posts/one",
		"extractor: trafilatura",
		"min_images: 2",
		"# opens:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the starter file is missing %q:\n%s", want, got)
		}
	}

	// min_chars is set below what was extracted, not equal to it: an assertion that
	// breaks when a body grows by one character is one nobody keeps.
	if !strings.Contains(got, "min_chars: 900") {
		t.Errorf("min_chars is not below the extracted length:\n%s", got)
	}

	// A page extraction currently fails on is worth capturing, and the file says
	// what to do about it rather than recording the failure as the expectation.
	failed := starterWant("https://example.com/posts/two", extract.Result{}, extract.ErrNoContent)
	if strings.Contains(failed, "min_chars") {
		t.Errorf("a failing page got a length assertion:\n%s", failed)
	}
	for _, want := range []string{"currently fails", "expect: none"} {
		if !strings.Contains(failed, want) {
			t.Errorf("the starter file does not explain the failure (%q):\n%s", want, failed)
		}
	}
}

// Bad invocations say what to do rather than failing obscurely.
func TestCorpusRejectsBadInvocations(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no action", nil, "Usage: tome corpus"},
		{"unknown action", []string{"remove", "x"}, "unknown action"},
		{"no url", []string{"add"}, "Usage: tome corpus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := corpusCmd(tc.args, &stdout, &stderr); code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr does not mention %q:\n%s", tc.want, stderr.String())
			}
		})
	}

	// Without the directory it refuses before reaching the network, and names the
	// variable that fixes it.
	t.Setenv(corpusDirEnv, "")
	var stdout, stderr bytes.Buffer
	if code := corpusCmd([]string{"add", "https://example.com/x"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), corpusDirEnv) {
		t.Errorf("stderr does not name %s:\n%s", corpusDirEnv, stderr.String())
	}
}

// A stem can only ever name a file inside the corpus directory.
//
// The derived ones are reduced to letters, digits and dashes by construction, but
// --name is whatever somebody typed — and "it happens to be safe today" is a
// different claim from "it cannot be otherwise".
func TestSafeStem(t *testing.T) {
	for _, tc := range []struct {
		stem string
		want bool
	}{
		{"arstechnica-a-slasher-fans-dream", true},
		{"page_01", true},
		{"", false},
		{"../../etc/passwd", false},
		{"nested/page", false},
		{"page.html", false},
		{"page name", false},
		{"Page", false},
	} {
		t.Run(tc.stem, func(t *testing.T) {
			if got := safeStem(tc.stem); got != tc.want {
				t.Errorf("safeStem(%q) = %v, want %v", tc.stem, got, tc.want)
			}
		})
	}

	// Everything corpusStem derives is usable, which is the property that makes the
	// check a guard on --name rather than a second implementation.
	for _, url := range []string{
		"https://arstechnica.com/culture/2026/08/a-slasher-fans-dream/",
		"https://example.com/Mixed_Case/Article!Name",
		"https://comics.example.com/comics/oots1347.html",
	} {
		if stem := corpusStem(url); !safeStem(stem) {
			t.Errorf("corpusStem(%q) = %q, which safeStem rejects", url, stem)
		}
	}
}
