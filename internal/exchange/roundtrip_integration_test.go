package exchange_test

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/exchange"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// snapshot is everything about one article that a round trip must preserve.
//
// Comparing whole snapshots rather than asserting field by field is the point: a
// field added to the archive and forgotten by the export shows up here as a
// difference, where a list of assertions would simply not mention it.
type snapshot struct {
	URLCanonical string
	Title        string
	Author       string
	SiteName     string
	Language     string
	PublishedAt  string
	SavedAt      string

	Read    bool
	Starred bool

	BodyText  string
	WordCount int

	Extractor        string
	ExtractorVersion string
	ContentOrigin    string
	Immutable        bool

	SourceName string
	SourceID   string

	Tags       []string
	Highlights []string
}

// snapshotArchive reads back everything an export claims to carry.
func snapshotArchive(t *testing.T, s *store.Store, userID store.UserID) map[string]snapshot {
	t.Helper()
	ctx := t.Context()

	out := make(map[string]snapshot)

	var cursor store.ArticleID
	for {
		rows, err := s.ExportArticles(ctx, userID, cursor, 100)
		if err != nil {
			t.Fatalf("ExportArticles() = %v", err)
		}
		if len(rows) == 0 {
			return out
		}

		for _, row := range rows {
			cursor = row.ArticleID

			snap := snapshot{
				URLCanonical:     row.URLCanonical,
				Title:            row.Title,
				Author:           row.Author,
				SiteName:         row.SiteName,
				Language:         row.Language,
				PublishedAt:      formatTime(row.PublishedAt),
				SavedAt:          formatTime(row.SavedAt),
				Read:             row.Read,
				Starred:          row.Starred,
				BodyText:         strings.TrimSpace(row.ContentText),
				WordCount:        row.WordCount,
				Extractor:        row.ExtractorName,
				ExtractorVersion: row.ExtractorVersion,
				ContentOrigin:    row.ContentOrigin,
				Immutable:        row.Immutable,
				SourceName:       row.SourceName,
				SourceID:         row.SourceID,
			}

			tags, err := s.TagsForArticle(ctx, userID, row.ArticleID)
			if err != nil {
				t.Fatalf("TagsForArticle() = %v", err)
			}
			for _, tag := range tags {
				snap.Tags = append(snap.Tags, tag.Name)
			}
			sort.Strings(snap.Tags)

			highlights, err := s.HighlightsForArticle(ctx, userID, row.ArticleID)
			if err != nil {
				t.Fatalf("HighlightsForArticle() = %v", err)
			}
			for _, h := range highlights {
				snap.Highlights = append(snap.Highlights, h.Quote+" | "+h.Note)
			}
			sort.Strings(snap.Highlights)

			out[row.URLCanonical] = snap
		}
	}
}

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// seedArchive builds an archive with one of everything a round trip has to carry.
func seedArchive(t *testing.T, s *store.Store, userID store.UserID) {
	t.Helper()
	ctx := t.Context()

	published := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	// A feed article with a fetched, mutable body: the ordinary case, and the one
	// whose replaceability must survive.
	feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://example.com/feed.xml", Title: "Example", Category: "Tech",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	fetched, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://example.com/posts/fetched",
		URLOriginal:  "https://example.com/posts/fetched?utm_source=feed",
		Title:        "A fetched article", Author: "A Writer",
		SiteName: "Example", Language: "en", PublishedAt: &published,
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
		FeedID: feedID, ArticleID: fetched, GUID: "guid-fetched",
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
	if _, err := s.InsertContent(ctx, store.ContentParams{
		ArticleID: fetched, ExtractorName: "trafilatura", ExtractorVersion: "3",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>The body of a fetched article.</p>",
		Text:          "The body of a fetched article.", WordCount: 6,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}
	if _, err := s.SetRead(ctx, userID, fetched, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}
	if _, err := s.SetStarred(ctx, userID, fetched, true); err != nil {
		t.Fatalf("SetStarred() = %v", err)
	}
	tagID, err := s.EnsureTag(ctx, userID, "worth-keeping")
	if err != nil {
		t.Fatalf("EnsureTag() = %v", err)
	}
	if _, err := s.TagArticle(ctx, userID, fetched, tagID); err != nil {
		t.Fatalf("TagArticle() = %v", err)
	}
	if _, err := s.AddHighlight(ctx, userID, fetched, store.ImportHighlight{
		Quote: "The body of a fetched article.", Note: "the passage that mattered",
	}); err != nil {
		t.Fatalf("AddHighlight() = %v", err)
	}

	// An imported article: immutable, with its original provenance, which must not
	// become "imported from tomekeeper" on the way back.
	if _, err := s.ImportArticle(ctx, userID, store.ImportParams{
		SourceName:   "wallabag",
		SourceID:     "4711",
		URLCanonical: "https://example.org/notes/imported",
		URLOriginal:  "https://example.org/notes/imported",
		Title:        "An imported article",
		ContentHTML:  "<p>Possibly the only copy.</p>",
		ContentText:  "Possibly the only copy.",
		WordCount:    4,
	}); err != nil {
		t.Fatalf("ImportArticle() = %v", err)
	}

	// A saved page with no body yet: the fetch has not happened, and the article
	// still has to survive the trip.
	if _, err := s.SaveArticle(ctx, userID, "https://example.net/pages/no-body-yet"); err != nil {
		t.Fatalf("SaveArticle() = %v", err)
	}
}

// export → fresh database → import reproduces the archive.
//
// The milestone's acceptance criterion, and the test the whole interchange format
// exists to make possible. It runs both halves against the same type, so a field
// that export forgets or import ignores is a difference in the comparison rather
// than a thing nobody notices for a year.
func TestRoundTripReproducesTheArchive(t *testing.T) {
	_, source, userID := dbtest.SetupWithUser(t)

	seedArchive(t, source, userID)
	before := snapshotArchive(t, source, userID)
	if len(before) != 3 {
		t.Fatalf("seeded %d articles, want 3", len(before))
	}

	var file bytes.Buffer
	result, err := exchange.Export(t.Context(), source, userID, &file)
	if err != nil {
		t.Fatalf("Export() = %v", err)
	}
	if result.Articles != 3 {
		t.Errorf("exported %d articles, want 3", result.Articles)
	}
	if result.WithoutBody != 1 {
		t.Errorf("reported %d articles without a body, want the 1 saved page", result.WithoutBody)
	}

	// The file is the format the importer detects, with no --format needed. That is
	// the symmetry claim, and it is cheap to check.
	imp := exchange.DetectImporterFor(file.Bytes())
	if imp == nil || imp.Name() != exchange.SourceTomekeeper {
		t.Fatalf("the export was not recognized as this archive's own format: %v", imp)
	}

	// A fresh database, which is what "restore" means: the same one, emptied. A
	// second Setup would deadlock on the per-test lock and would truncate away the
	// archive that was just exported.
	restored, restoredUser := emptyArchive(t, source)

	report, err := exchange.Apply(t.Context(), restored, restoredUser, imp,
		exchange.Source{Path: "export.json", Reader: bytes.NewReader(file.Bytes())})
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}
	if len(report.Written.Failures) > 0 {
		t.Fatalf("the restore failed on %d records: %v", len(report.Written.Failures), report.Written.Failures)
	}
	if report.Written.Articles != 3 {
		t.Errorf("restored %d articles, want 3", report.Written.Articles)
	}

	after := snapshotArchive(t, restored, restoredUser)

	if len(after) != len(before) {
		t.Fatalf("the restored archive holds %d articles, want %d", len(after), len(before))
	}
	for url, want := range before {
		got, ok := after[url]
		if !ok {
			t.Errorf("%s is missing from the restored archive", url)
			continue
		}
		compareSnapshots(t, url, want, got)
	}
}

// compareSnapshots reports every field that did not survive.
func compareSnapshots(t *testing.T, url string, want, got snapshot) {
	t.Helper()

	for _, field := range []struct {
		name      string
		want, got any
	}{
		{"title", want.Title, got.Title},
		{"author", want.Author, got.Author},
		{"site name", want.SiteName, got.SiteName},
		{"language", want.Language, got.Language},
		{"published", want.PublishedAt, got.PublishedAt},
		{"saved", want.SavedAt, got.SavedAt},
		{"read", want.Read, got.Read},
		{"starred", want.Starred, got.Starred},
		{"body text", want.BodyText, got.BodyText},
		{"word count", want.WordCount, got.WordCount},
		{"extractor", want.Extractor, got.Extractor},
		{"extractor version", want.ExtractorVersion, got.ExtractorVersion},
		{"content origin", want.ContentOrigin, got.ContentOrigin},
		{"immutable", want.Immutable, got.Immutable},
		{"tags", strings.Join(want.Tags, ","), strings.Join(got.Tags, ",")},
		{"highlights", strings.Join(want.Highlights, " / "), strings.Join(got.Highlights, " / ")},
	} {
		if field.want != field.got {
			t.Errorf("%s: %s did not survive the round trip: %v -> %v",
				url, field.name, field.want, field.got)
		}
	}

	// Provenance is the one thing a restore may legitimately add, and the rule is
	// worth stating rather than exempting.
	//
	// An article that came from somewhere else keeps that source exactly, so a later
	// re-import of the library it came from still recognizes it. An article this
	// archive collected itself had no source at all, and after a restore it has one:
	// this archive, keyed on the canonical URL. That is not a lost fact — it is a
	// true new one, and it is what lets a second restore of the same file know it
	// has already run.
	switch {
	case want.SourceName != "":
		if got.SourceName != want.SourceName || got.SourceID != want.SourceID {
			t.Errorf("%s: import provenance was lost: %s/%s -> %s/%s",
				url, want.SourceName, want.SourceID, got.SourceName, got.SourceID)
		}
	case got.SourceName != exchange.SourceTomekeeper || got.SourceID != want.URLCanonical:
		t.Errorf("%s: a restored article should be recorded as coming from this archive "+
			"keyed on its URL, got %s/%s", url, got.SourceName, got.SourceID)
	}
}

// The stored body survives a round trip byte for byte.
//
// The strongest thing a round trip can claim, and the one worth asserting
// separately: the HTML a reader sees is exactly what it was. Measured against the
// maintainer's real 385-article archive, all 341 bodies came back identical.
//
// What is *not* claimed is the derived text. It is recomputed from the sanitized
// body on the way in rather than carried, so where two block elements abut it can
// gain or lose a word boundary — 16 of those 341 bodies, differing by 46 words in
// total, all of the form "service.Data" against "service. Data". The words are the
// same; a couple of them touch. Fixing that means making text extraction insert
// boundaries at block elements, which changes what every extraction produces and is
// a decision with an extractor version bump attached, not a detail to slip in here.
func TestRoundTripPreservesTheBodyExactly(t *testing.T) {
	_, source, userID := dbtest.SetupWithUser(t)
	seedArchive(t, source, userID)

	bodies := bodiesByURL(t, source, userID)

	var file bytes.Buffer
	if _, err := exchange.Export(t.Context(), source, userID, &file); err != nil {
		t.Fatalf("Export() = %v", err)
	}

	restored, restoredUser := emptyArchive(t, source)
	if _, err := exchange.Apply(t.Context(), restored, restoredUser, exchange.Tomekeeper{},
		exchange.Source{Path: "export.json", Reader: bytes.NewReader(file.Bytes())}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	after := bodiesByURL(t, restored, restoredUser)

	for url, want := range bodies {
		got, ok := after[url]
		if !ok {
			t.Errorf("%s lost its body entirely", url)
			continue
		}
		if got != want {
			t.Errorf("%s: the stored body changed on the way back\nbefore: %s\nafter:  %s",
				url, want, got)
		}
	}
}

// bodiesByURL reads every current body, keyed by canonical URL.
func bodiesByURL(t *testing.T, s *store.Store, userID store.UserID) map[string]string {
	t.Helper()

	out := make(map[string]string)
	var cursor store.ArticleID
	for {
		rows, err := s.ExportArticles(t.Context(), userID, cursor, 100)
		if err != nil {
			t.Fatalf("ExportArticles() = %v", err)
		}
		if len(rows) == 0 {
			return out
		}
		for _, row := range rows {
			cursor = row.ArticleID
			if row.ContentHTML != "" {
				out[row.URLCanonical] = row.ContentHTML
			}
		}
	}
}

// A fetched body comes back fetched, not converted into an import.
//
// The difference is not cosmetic. An imported body is immutable: never re-extracted
// and never released by retention. If a restore turned every body into one, an
// archive restored from its own backup would be a different archive — every
// extraction improvement locked out of it, permanently, with no error anywhere.
func TestRoundTripKeepsBodiesReplaceable(t *testing.T) {
	_, source, userID := dbtest.SetupWithUser(t)
	seedArchive(t, source, userID)

	var file bytes.Buffer
	if _, err := exchange.Export(t.Context(), source, userID, &file); err != nil {
		t.Fatalf("Export() = %v", err)
	}

	restored, restoredUser := emptyArchive(t, source)
	if _, err := exchange.Apply(t.Context(), restored, restoredUser, exchange.Tomekeeper{},
		exchange.Source{Path: "export.json", Reader: bytes.NewReader(file.Bytes())}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	after := snapshotArchive(t, restored, restoredUser)

	fetched := after["https://example.com/posts/fetched"]
	if fetched.Immutable {
		t.Error("a fetched body came back immutable, so re-extraction could never improve it again")
	}
	if fetched.ContentOrigin != store.OriginFetched {
		t.Errorf("a fetched body came back with origin %q, want %q",
			fetched.ContentOrigin, store.OriginFetched)
	}
	if fetched.Extractor != "trafilatura" || fetched.ExtractorVersion != "3" {
		t.Errorf("the extractor that produced the body was lost: %s/%s",
			fetched.Extractor, fetched.ExtractorVersion)
	}

	imported := after["https://example.org/notes/imported"]
	if !imported.Immutable {
		t.Error("an imported body came back mutable, so retention could release the only copy")
	}
	// And it is still a Wallabag article, so re-importing that library recognizes
	// it rather than adding it a second time.
	if imported.SourceName != "wallabag" || imported.SourceID != "4711" {
		t.Errorf("import provenance was lost: %s/%s", imported.SourceName, imported.SourceID)
	}
}

// Restoring twice changes nothing, which is what makes a restore safe to retry.
func TestRoundTripRestoreIsIdempotent(t *testing.T) {
	_, source, userID := dbtest.SetupWithUser(t)
	seedArchive(t, source, userID)

	var file bytes.Buffer
	if _, err := exchange.Export(t.Context(), source, userID, &file); err != nil {
		t.Fatalf("Export() = %v", err)
	}

	restored, restoredUser := emptyArchive(t, source)
	for pass := 1; pass <= 2; pass++ {
		report, err := exchange.Apply(t.Context(), restored, restoredUser, exchange.Tomekeeper{},
			exchange.Source{Path: "export.json", Reader: bytes.NewReader(file.Bytes())})
		if err != nil {
			t.Fatalf("Apply() pass %d = %v", pass, err)
		}
		if len(report.Written.Failures) > 0 {
			t.Fatalf("pass %d failed on %d records", pass, len(report.Written.Failures))
		}
		if pass == 2 && report.Written.Articles != 0 {
			t.Errorf("a second restore wrote %d articles, want 0", report.Written.Articles)
		}
	}

	var bodies int64
	if err := restored.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM article_content`).Scan(&bodies); err != nil {
		t.Fatalf("counting bodies: %v", err)
	}
	// Two of the three seeded articles have a body; a second restore must not stack
	// another copy of either.
	if bodies != 2 {
		t.Errorf("%d bodies after restoring twice, want 2", bodies)
	}
}

// The export is a JSON array a person can read, and every record validates.
func TestExportIsReadableJSON(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	seedArchive(t, s, userID)

	var file bytes.Buffer
	if _, err := exchange.Export(t.Context(), s, userID, &file); err != nil {
		t.Fatalf("Export() = %v", err)
	}

	text := file.String()
	if !strings.HasPrefix(text, "[\n") || !strings.HasSuffix(text, "]\n") {
		t.Errorf("the export is not a JSON array:\n%s", truncateForTest(text))
	}
	// Indented, because this file is meant to be opened in an editor a decade from
	// now, and the whitespace costs nothing beside the articles.
	if !strings.Contains(text, "\n    \"url\":") {
		t.Errorf("the export is not indented for reading:\n%s", truncateForTest(text))
	}

	// An empty archive is still a valid document rather than an empty file.
	empty, emptyUser := emptyArchive(t, s)
	var emptyFile bytes.Buffer
	if _, err := exchange.Export(t.Context(), empty, emptyUser, &emptyFile); err != nil {
		t.Fatalf("Export() on an empty archive = %v", err)
	}
	if got := strings.TrimSpace(emptyFile.String()); got != "[\n]" {
		t.Errorf("an empty archive exported as %q", got)
	}
	if imp := exchange.DetectImporterFor(emptyFile.Bytes()); imp != nil {
		// Nothing to detect in an empty array, and that is honest rather than a
		// problem: there are no records to identify a format by.
		t.Logf("an empty export is detected as %s", imp.Name())
	}
}

// Written to a file, read back from it: the shape an operator actually uses.
func TestExportToFileAndBack(t *testing.T) {
	_, source, userID := dbtest.SetupWithUser(t)
	seedArchive(t, source, userID)

	path := filepath.Join(t.TempDir(), "archive.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating the export: %v", err)
	}
	if _, err := exchange.Export(t.Context(), source, userID, file); err != nil {
		t.Fatalf("Export() = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("closing the export: %v", err)
	}

	imp, err := exchange.DetectImporter(path)
	if err != nil {
		t.Fatalf("DetectImporter() = %v", err)
	}
	if imp == nil || imp.Name() != exchange.SourceTomekeeper {
		t.Fatalf("the file was not recognized as an export: %v", imp)
	}

	reopened, err := os.Open(path)
	if err != nil {
		t.Fatalf("reopening the export: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	restored, restoredUser := emptyArchive(t, source)
	report, err := exchange.Apply(t.Context(), restored, restoredUser, imp,
		exchange.Source{Path: path, Reader: reopened})
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}
	if report.Written.Articles != 3 {
		t.Errorf("restored %d articles from the file, want 3", report.Written.Articles)
	}
}

// emptyArchive clears the archive and re-seeds its reader, which is what a restore
// arrives into.
func emptyArchive(t *testing.T, s *store.Store) (*store.Store, store.UserID) {
	t.Helper()

	dbtest.Empty(t, s.Pool())

	userID, _, err := s.System().EnsureSeedUser(t.Context(), "tome")
	if err != nil {
		t.Fatalf("re-seeding the reader: %v", err)
	}
	return s, userID
}

func truncateForTest(s string) string {
	if len(s) <= 600 {
		return s
	}
	return s[:600] + "…"
}
