package exchange_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/exchange"
)

// The image census is what an import report says about what an archive will look
// like when it finishes, so each class has to mean what it claims.
func TestImageCensusClassifiesRealMarkup(t *testing.T) {
	articles, errs := read(t, "library.json")
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}

	f, err := os.Open(filepath.Join(fixtures, "library.json"))
	if err != nil {
		t.Fatalf("opening the fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	report, err := exchange.Inspect(t.Context(), nil, 0, exchange.Wallabag{},
		exchange.Source{Path: "library.json", Reader: f})
	if err != nil {
		t.Fatalf("Inspect() = %v", err)
	}

	c := report.Images

	// Two from the first record: one absolute, one relative. A relative reference
	// counts as fetchable because the body is resolved against the article's own
	// URL before it is stored, which turns it into an address on the origin site.
	// One more from the last record, whose second image is the origin site's own
	// /assets/images/ layout rather than the exporting installation's storage.
	if c.Fetchable != 3 {
		t.Errorf("Fetchable = %d, want 3", c.Fetchable)
	}

	// The relative /assets/images/ reference, which lives inside the exporting
	// installation and cannot be reached from here. This is the download_images=on
	// case the import has to warn about rather than silently lose.
	if c.InSourceStorage != 1 {
		t.Errorf("InSourceStorage = %d, want 1", c.InSourceStorage)
	}

	// The data: URI needs no fetch.
	if c.SelfContained != 1 {
		t.Errorf("SelfContained = %d, want 1", c.SelfContained)
	}

	// The `denied:data:` placeholder is not an address this archive will store, and
	// counting it as an image to fetch would inflate the promise.
	if c.Unusable != 1 {
		t.Errorf("Unusable = %d, want 1", c.Unusable)
	}

	if c.Total() != 6 {
		t.Errorf("Total() = %d, want 6", c.Total())
	}

	// Three records carry images; the other three carry none.
	if report.WithImages != 3 {
		t.Errorf("WithImages = %d, want 3", report.WithImages)
	}
	if report.Records != len(articles) {
		t.Errorf("Records = %d, want %d", report.Records, len(articles))
	}
}

// Inspect works with no database, which is what makes the file half of a report
// available before anything is set up.
func TestInspectWithoutADatabaseReportsTheFile(t *testing.T) {
	f, err := os.Open(filepath.Join(fixtures, "library.json"))
	if err != nil {
		t.Fatalf("opening the fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	report, err := exchange.Inspect(t.Context(), nil, 0, exchange.Wallabag{},
		exchange.Source{Path: "library.json", Reader: f})
	if err != nil {
		t.Fatalf("Inspect() = %v", err)
	}

	if report.Records != 6 {
		t.Errorf("Records = %d, want 6", report.Records)
	}
	if report.WithoutBody != 2 || report.PlaceholderBodies != 1 {
		t.Errorf("without a body = %d (%d placeholders), want 2 (1)",
			report.WithoutBody, report.PlaceholderBodies)
	}
	if report.Tags != 4 {
		t.Errorf("Tags = %d, want 4", report.Tags)
	}
	if report.Highlights != 1 {
		t.Errorf("Highlights = %d, want 1", report.Highlights)
	}

	// Without a database there is nothing to say about what is already imported,
	// and the report does not invent it.
	if report.New != 0 || report.AlreadyImported != 0 || report.DuplicateURLs != 0 {
		t.Errorf("a database-free report claimed new=%d already=%d duplicates=%d",
			report.New, report.AlreadyImported, report.DuplicateURLs)
	}
}

// A truncated file fails Inspect, which is what keeps a corrupt export from ever
// reaching the write pass.
func TestInspectFailsOnATruncatedExport(t *testing.T) {
	f, err := os.Open(filepath.Join(fixtures, "truncated.json"))
	if err != nil {
		t.Fatalf("opening the fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := exchange.Inspect(t.Context(), nil, 0, exchange.Wallabag{},
		exchange.Source{Path: "truncated.json", Reader: f}); err == nil {
		t.Fatal("Inspect() accepted a truncated export")
	}
}
