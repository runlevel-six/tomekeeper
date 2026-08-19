package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// Argument errors are reported before anything reaches for a database, so a
// typo does not turn into a connection error that hides it.
func TestExplainRejectsBadInvocations(t *testing.T) {
	t.Setenv("TOME_DATABASE_URL", "")

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no article", []string{"explain"}, "Usage: tome explain"},
		{"two articles", []string{"explain", "1", "2"}, "Usage: tome explain"},
		{"not a number", []string{"explain", "the-mvp-post"}, "is not an article id"},
		{"zero", []string{"explain", "0"}, "is not an article id"},
		// flag takes "-3" for a flag, not an argument, so this fails one step
		// earlier than the others. Still usage, still the usage text.
		{"negative", []string{"explain", "-3"}, "Usage: tome explain"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if got := run(tt.args, &stdout, &stderr); got != exitUsage {
				t.Errorf("run(%q) = %d, want %d\nstderr: %s", tt.args, got, exitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

// The output is the whole product here, so it is worth asserting rather than
// eyeballing: a failure has to name every rung, what each produced, and the
// threshold that turned it down.
func TestPrintExplanationOfAFailure(t *testing.T) {
	article := store.Article{
		ID:           1267,
		URLCanonical: "https://example.com/2026/08/a-post",
		RawBlobPath:  "pages/ab/cd/abcdef.html.gz",
		FetchStatus:  "failed",
		FetchError:   "extraction produced no content",
	}
	in := extract.Input{RawHTML: []byte("<html></html>"), URL: article.URLCanonical}
	steps := []extract.Step{
		{Rung: "page", Ran: true, Chars: 41904, Why: "41904 characters of visible text; a body under 2000 characters must be at least 25% of it (10476 characters)"},
		{Rung: extract.NameDomainRule, Why: "no rule for this domain"},
		{Rung: extract.NameTrafilatura, Ran: true, Why: "produced nothing"},
		{Rung: extract.NameReadability, Ran: true, Chars: 148, Why: "148 characters, under the 200-character floor"},
		{Rung: extract.NameFeedBody, Why: "the feed carried no body for this article"},
		{Rung: extract.NamePageImages, Ran: true, Why: "no image on the page carries this article's slug, so none of them is its content"},
	}

	var out bytes.Buffer
	printExplanation(&out, article, in, 132456, extract.Result{}, steps, extract.ErrNoContent, false)
	got := out.String()

	for _, want := range []string{
		"article 1267",
		article.URLCanonical,
		article.RawBlobPath,
		"fetch: failed",
		// Every rung, including the two that never ran: "readability was
		// skipped" is an answer, and a missing row reads as an omission the
		// operator then has to go and check in the source.
		extract.NameDomainRule,
		extract.NameTrafilatura,
		extract.NameReadability,
		extract.NameFeedBody,
		extract.NamePageImages,
		"skipped",
		"under the 200-character floor",
		// And what to do about it, which is the point of running this at all.
		"domain rule naming the element",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the explanation does not mention %q:\n%s", want, got)
		}
	}

	if strings.Contains(got, "ACCEPTED") {
		t.Errorf("a failed extraction reported an accepted rung:\n%s", got)
	}
}

// A success says which rung won and how much it produced. Without it, the only
// way to tell a domain rule is working is to read the article.
func TestPrintExplanationOfASuccess(t *testing.T) {
	article := store.Article{
		ID:           8607,
		URLCanonical: "https://example.com/feature",
		RawBlobPath:  "pages/12/34/123456.html.gz",
		FetchStatus:  "fetched",
	}
	in := extract.Input{
		RawHTML: []byte("<html></html>"),
		URL:     article.URLCanonical,
		Rule: &extract.Rule{
			ContentSelector: `div.relative > div.ars-lightbox, .post-content`,
			StripSelectors:  []string{"img.hidden", ".ad-wrapper"},
		},
	}
	steps := []extract.Step{
		{Rung: "page", Ran: true, Chars: 40000, Why: "40000 characters of visible text"},
		{Rung: extract.NameDomainRule, Ran: true, Chars: 6021, Words: 1062, Images: 2, Accepted: true,
			Why: "the rule's selector (div.relative > div.ars-lightbox, .post-content) matched, and a rule overrides the ratio check"},
	}

	var out bytes.Buffer
	printExplanation(&out, article, in, 210000, extract.Result{Name: extract.NameDomainRule, WordCount: 1062}, steps, nil, false)
	got := out.String()

	for _, want := range []string{
		"ACCEPTED",
		"1062",
		// Printed so an operator can paste it back into the rule, which means
		// the quotes inside an attribute selector must survive intact.
		`div.relative > div.ars-lightbox, .post-content`,
		"2 strip selector(s)",
		"Result: " + extract.NameDomainRule,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the explanation does not mention %q:\n%s", want, got)
		}
	}
}

// An article with nothing stored is the shape every import has, and the report
// has to say so rather than printing a blank path.
func TestPrintExplanationWithNoStoredPage(t *testing.T) {
	article := store.Article{ID: 42, URLCanonical: "https://example.com/imported", FetchStatus: "pending"}

	var out bytes.Buffer
	printExplanation(&out, article, extract.Input{FeedBody: "<p>a summary</p>"}, 0,
		extract.Result{}, []extract.Step{
			{Rung: "page", Why: "no stored page, so only the feed body can produce anything"},
		}, errors.New("boom"), false)

	if got := out.String(); !strings.Contains(got, "nothing was fetched") {
		t.Errorf("the explanation does not say the page was never fetched:\n%s", got)
	}
}
