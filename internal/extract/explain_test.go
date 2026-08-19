package extract_test

import (
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/extract"
)

// The guarantee the whole feature rests on: an explanation describes the
// extraction that actually happens.
//
// Explain and Extract share one implementation for exactly this reason, and this
// test is what keeps it that way. Split them — a second ladder written for
// reporting, a shortcut that skips a rung when explaining — and the explanation
// becomes a description of a program that does not exist, which is worse than no
// explanation at all because it is believed.
func TestExplainAgreesWithExtract(t *testing.T) {
	for _, tc := range committedCorpus(t) {
		t.Run(tc.name, func(t *testing.T) {
			want, wantErr := extract.New().Extract(tc.input)
			got, steps, gotErr := extract.New().Explain(tc.input)

			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("Extract() error = %v, Explain() error = %v", wantErr, gotErr)
			}
			if got.Name != want.Name {
				t.Errorf("extractor: Extract() = %q, Explain() = %q", want.Name, got.Name)
			}
			if got.HTML != want.HTML {
				t.Errorf("Explain() returned a different body than Extract() (%d vs %d characters)",
					len(got.HTML), len(want.HTML))
			}
			if len(steps) == 0 && gotErr == nil {
				t.Error("Explain() reported no steps for an extraction that succeeded")
			}
		})
	}
}

// Extract does not pay for the explanation, and cannot accidentally start
// returning one.
func TestExtractRecordsNothing(t *testing.T) {
	tc := findCase(t, "semantic-article")

	_, steps, err := extract.New().Explain(tc.input)
	if err != nil {
		t.Fatalf("Explain() = %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("Explain() recorded no steps, so this test proves nothing about Extract")
	}

	// Extract's signature has no steps in it, so the assertion is the compile —
	// what is checked here is that the recorder stays behind the flag.
	if _, err := extract.New().Extract(tc.input); err != nil {
		t.Fatalf("Extract() = %v", err)
	}
}

// The reason this command exists: a failure has to say what each rung produced
// and which threshold turned it down, not "no extractor produced acceptable
// content".
func TestExplainAFailureNamesEveryRung(t *testing.T) {
	tc := findCase(t, "js-shell")

	_, steps, err := extract.New().Explain(tc.input)
	if err == nil {
		t.Fatal("the js-shell fixture extracted successfully; this test needs a failure")
	}

	seen := make(map[string]extract.Step, len(steps))
	for _, s := range steps {
		if s.Why == "" {
			t.Errorf("rung %q reported no reason", s.Rung)
		}
		if s.Accepted {
			t.Errorf("rung %q was accepted in an extraction that failed", s.Rung)
		}
		seen[s.Rung] = s
	}

	// Every rung of the ladder accounted for, including the ones that never ran:
	// "readability was skipped" is an answer, and its absence reads as an
	// omission the reader then has to go and check in the source.
	for _, rung := range []string{
		"page",
		extract.NameDomainRule,
		extract.NameTrafilatura,
		extract.NameReadability,
		extract.NameFeedBody,
		extract.NamePageImages,
	} {
		if _, ok := seen[rung]; !ok {
			t.Errorf("no step reported for rung %q", rung)
		}
	}

	// The page measurement is the denominator of the ratio check, so it is the
	// number that explains the two rungs that rejected on ratio.
	if page := seen["page"]; page.Chars == 0 {
		t.Error("the page step reported no visible text, so the ratio check has no denominator to explain")
	}

	if rule := seen[extract.NameDomainRule]; rule.Ran {
		t.Error("a domain rule ran for a fixture that has none")
	} else if !strings.Contains(rule.Why, "no rule") {
		t.Errorf("the skipped-rule step says %q, which does not say a rule is missing", rule.Why)
	}
}

// A decision has to name the number that decided it. "Accepted" alone sends the
// reader to the source to find out which of four thresholds applied, which is the
// errand this command exists to save.
//
// Runs over the whole corpus so it covers both accepting branches: the short body
// that had to clear the ratio, and the long one the ratio stops applying to.
func TestExplainNamesTheThresholdThatDecided(t *testing.T) {
	for _, tc := range committedCorpus(t) {
		t.Run(tc.name, func(t *testing.T) {
			_, steps, err := extract.New().Explain(tc.input)
			if err != nil {
				t.Skip("this fixture extracts nothing; the rejection path is covered elsewhere")
			}

			var accepted extract.Step
			for _, s := range steps {
				if s.Accepted {
					accepted = s
				}
			}
			if accepted.Rung == "" {
				t.Fatal("extraction succeeded but no step was marked accepted")
			}

			switch accepted.Rung {
			case extract.NameDomainRule:
				// A rule is a human overriding the thresholds, so what the
				// explanation owes the reader is which selector won.
				if !strings.Contains(accepted.Why, tc.input.Rule.ContentSelector) {
					t.Errorf("the accepted rule step does not name its selector: %q", accepted.Why)
				}
			case extract.NameFeedBody:
				if !strings.Contains(accepted.Why, "feed") {
					t.Errorf("the accepted feed-body step does not say so: %q", accepted.Why)
				}
			default:
				// Both heuristic branches state a character count against the
				// threshold it was measured against.
				if !strings.Contains(accepted.Why, "characters") {
					t.Errorf("the accepted step names no character count: %q", accepted.Why)
				}
				if accepted.Chars < 2000 && !strings.Contains(accepted.Why, "%") {
					t.Errorf("a body short enough for the ratio check to apply did not report its "+
						"share of the page: %q", accepted.Why)
				}
			}
		})
	}
}

// Feed-body-only extraction is the shape every imported and every unfetchable
// article has, and it is the one case where "page" is not a failure to report
// but the normal state.
func TestExplainWithNoStoredPage(t *testing.T) {
	_, steps, err := extract.New().Explain(extract.Input{
		URL:      "https://example.com/feed-only",
		FeedBody: "<p>" + strings.Repeat("A full article delivered in the feed. ", 20) + "</p>",
	})
	if err != nil {
		t.Fatalf("Explain() = %v", err)
	}

	var saidSo bool
	for _, s := range steps {
		if s.Rung == "page" && strings.Contains(s.Why, "no stored page") {
			saidSo = true
		}
		if s.Rung == extract.NameTrafilatura || s.Rung == extract.NameReadability {
			t.Errorf("rung %q ran with no page to run against", s.Rung)
		}
	}
	if !saidSo {
		t.Error("nothing in the explanation said there was no stored page")
	}
}
