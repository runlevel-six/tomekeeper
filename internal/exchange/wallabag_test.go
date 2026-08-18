package exchange_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/exchange"
)

const fixtures = "testdata/imports/wallabag"

// read runs the adapter over a fixture and returns what it produced, keeping the
// per-record errors separate from the articles.
func read(t *testing.T, name string) ([]*exchange.Article, []error) {
	t.Helper()

	f, err := os.Open(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatalf("opening the fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	var (
		articles []*exchange.Article
		errs     []error
	)
	for a, err := range (exchange.Wallabag{}).Import(t.Context(),
		exchange.Source{Path: name, Reader: f}) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		articles = append(articles, a)
	}
	return articles, errs
}

func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("bad time in the test itself: %v", err)
	}
	return parsed.UTC()
}

// The golden mapping. Every assertion here is a decision about what one system's
// field means in this one, which is the whole substance of an adapter.
func TestWallabagMapsARealLibrary(t *testing.T) {
	articles, errs := read(t, "library.json")
	if len(errs) != 0 {
		t.Fatalf("the fixture produced %d errors, want none: %v", len(errs), errs)
	}
	if len(articles) != 6 {
		t.Fatalf("read %d articles, want 6", len(articles))
	}

	first := articles[0]

	// The submitted URL is kept as the original and the resolved one recorded
	// separately, because they differ and both are worth having: one is what the
	// reader would recognize, the other is what the archive keys on.
	if first.URL != "https://example.com/posts/the-ordinary-case?utm_source=newsletter" {
		t.Errorf("URL = %q, want the URL as submitted", first.URL)
	}
	if first.ResolvedURL != "https://example.com/posts/the-ordinary-case" {
		t.Errorf("ResolvedURL = %q, want the URL the source settled on", first.ResolvedURL)
	}

	if first.SourceName != exchange.SourceWallabag || first.SourceID != "101" {
		t.Errorf("source = %s/%s, want wallabag/101", first.SourceName, first.SourceID)
	}
	if first.SiteName != "example.com" {
		t.Errorf("SiteName = %q, want the source's domain_name", first.SiteName)
	}

	// A POSIX locale becomes a language tag, because that is what belongs in a
	// lang attribute.
	if first.Language != "en-US" {
		t.Errorf("Language = %q, want en-US", first.Language)
	}

	// Several authors become one byline, since the archive has one author field.
	if first.Author != "A Writer, Another Writer" {
		t.Errorf("Author = %q, want both names joined", first.Author)
	}

	if first.PublishedAt == nil || !first.PublishedAt.Equal(at(t, "2019-03-01T00:00:00Z")) {
		t.Errorf("PublishedAt = %v, want the published date", first.PublishedAt)
	}
	// created_at is when the reader saved it, which is the date that keeps a
	// ten-year library in its own order rather than arriving all at once.
	if first.SavedAt == nil || !first.SavedAt.Equal(at(t, "2019-03-04T09:15:00Z")) {
		t.Errorf("SavedAt = %v, want the source's created_at", first.SavedAt)
	}

	// "Archived" in Wallabag is what every other reader calls read.
	if !first.Read || !first.Archived || !first.Starred {
		t.Errorf("read/archived/starred = %v/%v/%v, want all true",
			first.Read, first.Archived, first.Starred)
	}

	if strings.Join(first.Tags, ",") != "reading,archives" {
		t.Errorf("Tags = %v, want the two string tags", first.Tags)
	}

	if len(first.Highlights) != 1 {
		t.Fatalf("read %d highlights, want 1", len(first.Highlights))
	}
	h := first.Highlights[0]
	if !strings.HasPrefix(h.Quote, "The best time to keep a copy") {
		t.Errorf("highlight quote = %q", h.Quote)
	}
	if h.Note != "This is the whole argument." {
		t.Errorf("highlight note = %q, want the annotation's text", h.Note)
	}
	if h.CreatedAt == nil || !h.CreatedAt.Equal(at(t, "2019-03-05T11:00:00Z")) {
		t.Errorf("highlight created = %v", h.CreatedAt)
	}
}

// The source's own fetch-failure message is not a body.
//
// This is the single most consequential judgement in the adapter. An imported body
// is immutable, so storing this placeholder would make a paragraph of another
// program's error message the permanent, unreplaceable content of the article —
// and would count it as a successful import. In the maintainer's real library it is
// 42 of 385 records.
func TestWallabagRefusesItsOwnFetchFailureAsABody(t *testing.T) {
	articles, _ := read(t, "library.json")

	placeholder := articles[1]
	if placeholder.SourceID != "102" {
		t.Fatalf("fixture order changed; record 2 is %s", placeholder.SourceID)
	}
	if placeholder.ContentHTML != "" {
		t.Errorf("the placeholder was imported as a body:\n%s", placeholder.ContentHTML)
	}
	if !placeholder.PlaceholderBody {
		t.Error("the placeholder was not reported as one, so a report cannot mention it")
	}

	// And an ordinary empty body is not mistaken for a placeholder: the two are
	// different pieces of news about somebody's library.
	empty := articles[4]
	if empty.SourceID != "105" {
		t.Fatalf("fixture order changed; record 5 is %s", empty.SourceID)
	}
	if empty.ContentHTML != "" || empty.PlaceholderBody {
		t.Errorf("a record with no content field: body %q, placeholder %v",
			empty.ContentHTML, empty.PlaceholderBody)
	}
}

// An article *about* the failure is still an article. The marker only counts in a
// body short enough to be the placeholder itself.
func TestWallabagKeepsAnArticleThatQuotesTheFailureMarker(t *testing.T) {
	body := "<p>Long article about fetching.</p>" +
		strings.Repeat("<p>Some prose that goes on for a while.</p>", 200) +
		`<p>See <a href="https://doc.wallabag.org/en/user/errors_during_fetching.html">the docs</a>.</p>`

	export := `[{"id":1,"url":"https://example.com/a","title":"On fetch failures",` +
		`"is_archived":0,"is_starred":0,"tags":[],"annotations":[],"content":` +
		quote(body) + `}]`

	articles, errs := readString(t, export)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(articles) != 1 || articles[0].ContentHTML == "" {
		t.Fatalf("an article quoting the marker lost its body")
	}
}

// Flags and dates written the older way still read.
func TestWallabagReadsOlderFieldSpellings(t *testing.T) {
	articles, _ := read(t, "library.json")

	older := articles[2]
	if older.SourceID != "103" {
		t.Fatalf("fixture order changed; record 3 is %s", older.SourceID)
	}
	if !older.Read || older.Starred {
		t.Errorf("boolean flags: read %v, starred %v; want true, false", older.Read, older.Starred)
	}
	if older.SavedAt == nil || !older.SavedAt.Equal(at(t, "2016-11-02T08:30:00Z")) {
		t.Errorf("SavedAt = %v, want the zoneless date read as UTC", older.SavedAt)
	}

	// Tags as objects, falling back to the slug when there is no label.
	if strings.Join(older.Tags, ",") != "Tags As Objects,label-missing" {
		t.Errorf("Tags = %v, want the label then the slug", older.Tags)
	}
}

// A date that cannot be read costs the date, not the record.
func TestWallabagKeepsARecordWithAnUnreadableDate(t *testing.T) {
	articles, _ := read(t, "library.json")

	localized := articles[5]
	if localized.SourceID != "106" {
		t.Fatalf("fixture order changed; record 6 is %s", localized.SourceID)
	}
	if localized.PublishedAt != nil {
		t.Errorf("PublishedAt = %v, want nil for a date that is not one", localized.PublishedAt)
	}
	if localized.SavedAt == nil {
		t.Error("a readable created_at was lost along with the unreadable published_at")
	}

	// A highlight with nothing quoted is dropped: there is no text to reconcile
	// against a body this archive extracted, which is the whole reason highlights
	// travel as text rather than as offsets.
	if len(localized.Highlights) != 0 {
		t.Errorf("kept %d highlights with no quoted text, want 0", len(localized.Highlights))
	}
}

// One unreadable record does not cost the rest of the library.
func TestWallabagSkipsOneBadRecordAndKeepsGoing(t *testing.T) {
	articles, errs := read(t, "onebadrecord.json")

	if len(articles) != 2 {
		t.Errorf("read %d articles, want the 2 readable ones", len(articles))
	}
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(errs), errs)
	}

	// The position is in the message, because a record in a 6MB single-line export
	// cannot be found any other way.
	if !strings.Contains(errs[0].Error(), "record 2") {
		t.Errorf("the error does not say which record failed: %v", errs[0])
	}

	// And it is not fatal: the file was read to the end.
	var fatal *exchange.FatalError
	if errors.As(errs[0], &fatal) {
		t.Error("a single bad record was reported as ending the file")
	}
}

// A truncated export fails, and fails as fatal.
//
// The distinction matters more than it looks: reported as a per-record problem, a
// file cut off after 200 of 9,000 entries would import 200 articles and print a
// cheerful summary. Fatal means the import stops and says the file is incomplete.
func TestWallabagTreatsATruncatedFileAsFatal(t *testing.T) {
	_, errs := read(t, "truncated.json")

	if len(errs) == 0 {
		t.Fatal("a truncated export produced no error")
	}

	last := errs[len(errs)-1]
	var fatal *exchange.FatalError
	if !errors.As(last, &fatal) {
		t.Errorf("a truncated export was not reported as fatal: %v", last)
	}
}

// A file cut off *between* records is the dangerous truncation, and it fails too.
//
// This is the one that could look like success. Every record in the file is
// complete and readable, so the adapter yields them all without complaint; the only
// evidence that a library was cut in half is the closing bracket that never
// arrives. An importer that stopped at the last readable record would import 200
// articles of 9,000 and print a cheerful summary — so the end of the array is
// checked explicitly, and its absence is fatal.
func TestWallabagTreatsAFileCutBetweenRecordsAsFatal(t *testing.T) {
	articles, errs := read(t, "cutbetweenrecords.json")

	// Both records were readable, which is exactly why the file needs checking.
	if len(articles) != 2 {
		t.Errorf("read %d articles, want the 2 complete ones", len(articles))
	}
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(errs), errs)
	}

	var fatal *exchange.FatalError
	if !errors.As(errs[0], &fatal) {
		t.Errorf("a file with no closing bracket was not fatal: %v", errs[0])
	}
	if !strings.Contains(errs[0].Error(), "ends before its last record") {
		t.Errorf("the error does not say the file is incomplete: %v", errs[0])
	}

	// And Inspect refuses it, which is what keeps it out of the write pass.
	f, err := os.Open(filepath.Join(fixtures, "cutbetweenrecords.json"))
	if err != nil {
		t.Fatalf("opening the fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := exchange.Inspect(t.Context(), nil, 0, exchange.Wallabag{},
		exchange.Source{Path: "cutbetweenrecords.json", Reader: f}); err == nil {
		t.Error("Inspect() accepted a file with no closing bracket")
	}
}

// Detection reads the fields, not the filename.
func TestWallabagDetection(t *testing.T) {
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"library.json", true},
		{"onebadrecord.json", true},
		{"truncated.json", true}, // recognizable even though it is unreadable
		{"notanexport.txt", false},
	} {
		t.Run(tc.file, func(t *testing.T) {
			got, err := (exchange.Wallabag{}).Detect(filepath.Join(fixtures, tc.file))
			if err != nil {
				t.Fatalf("Detect() = %v", err)
			}
			if got != tc.want {
				t.Errorf("Detect(%s) = %v, want %v", tc.file, got, tc.want)
			}
		})
	}

	// A missing file is an error rather than "not this format": the operator named
	// it, so being unable to open it is worth saying plainly.
	if _, err := (exchange.Wallabag{}).Detect(filepath.Join(fixtures, "nothing-here.json")); err == nil {
		t.Error("detecting a missing file reported no error")
	}
}

func TestDetectImporterPicksTheAdapter(t *testing.T) {
	imp, err := exchange.DetectImporter(filepath.Join(fixtures, "library.json"))
	if err != nil {
		t.Fatalf("DetectImporter() = %v", err)
	}
	if imp == nil || imp.Name() != exchange.SourceWallabag {
		t.Fatalf("DetectImporter() = %v, want the wallabag adapter", imp)
	}

	// An unrecognized file is not an error — the caller turns it into a message
	// naming the formats this build does read.
	imp, err = exchange.DetectImporter(filepath.Join(fixtures, "notanexport.txt"))
	if err != nil {
		t.Fatalf("DetectImporter() on prose = %v, want no error", err)
	}
	if imp != nil {
		t.Errorf("DetectImporter() on prose = %v, want nothing", imp)
	}
}

func TestImporterNamed(t *testing.T) {
	if _, ok := exchange.ImporterNamed("WALLABAG"); !ok {
		t.Error("--format is case sensitive; it should not be")
	}
	if _, ok := exchange.ImporterNamed("pocket"); ok {
		t.Error("a format this build cannot read was reported as available")
	}
}

// readString runs the adapter over an inline export.
func readString(t *testing.T, export string) ([]*exchange.Article, []error) {
	t.Helper()

	var (
		articles []*exchange.Article
		errs     []error
	)
	for a, err := range (exchange.Wallabag{}).Import(t.Context(),
		exchange.Source{Path: "inline", Reader: strings.NewReader(export)}) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		articles = append(articles, a)
	}
	return articles, errs
}

// quote makes a JSON string literal out of a body.
func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}
