package exchange_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/exchange"
	"github.com/runlevel-six/tomekeeper/internal/store"
	"github.com/runlevel-six/tomekeeper/internal/urlcanon"
)

// apply imports the fixture library, from a fresh reader each time so that two
// runs in one test cannot share a file position.
func apply(t *testing.T, s *store.Store, userID store.UserID) exchange.Report {
	t.Helper()

	f, err := os.Open(filepath.Join(fixtures, "library.json"))
	if err != nil {
		t.Fatalf("opening the fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	report, err := exchange.Apply(t.Context(), s, userID, exchange.Wallabag{},
		exchange.Source{Path: "library.json", Reader: f})
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}
	if report.Written == nil {
		t.Fatal("Apply() reported nothing about what it wrote")
	}
	if len(report.Written.Failures) > 0 {
		t.Fatalf("Apply() failed on %d records: %v",
			len(report.Written.Failures), report.Written.Failures)
	}
	return report
}

// articleAt finds the article a fixture URL landed on.
func articleAt(t *testing.T, s *store.Store, userID store.UserID, rawURL string) store.ArticleID {
	t.Helper()

	canonical, err := urlcanon.Canonicalize(rawURL)
	if err != nil {
		t.Fatalf("canonicalizing %q: %v", rawURL, err)
	}
	id, found, err := s.ArticleVisibleByURL(t.Context(), userID, canonical)
	if err != nil {
		t.Fatalf("ArticleVisibleByURL() = %v", err)
	}
	if !found {
		t.Fatalf("%s is not in the archive", canonical)
	}
	return id
}

func countRows(t *testing.T, ctx context.Context, s *store.Store, sql string, args ...any) int64 {
	t.Helper()

	var n int64
	if err := s.Pool().QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	return n
}

func TestImportWritesALibrary(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	report := apply(t, s, userID)

	if report.Written.Articles != 6 {
		t.Errorf("imported %d articles, want 6", report.Written.Articles)
	}
	// Four of the six records carry a body: one is the source's fetch-failure
	// placeholder and one has no content field at all.
	if report.Written.Bodies != 4 {
		t.Errorf("stored %d bodies, want 4", report.Written.Bodies)
	}
	if report.Written.QueuedForFetch != 2 {
		t.Errorf("queued %d articles for fetching, want the 2 with no usable body",
			report.Written.QueuedForFetch)
	}
	if report.Written.TagsAdded != 4 {
		t.Errorf("added %d tags, want 4", report.Written.TagsAdded)
	}
	if report.Written.HighlightsAdded != 1 {
		t.Errorf("added %d highlights, want 1", report.Written.HighlightsAdded)
	}

	// The article the whole mapping test is about, now in the archive.
	first := articleAt(t, s, userID, "https://example.com/posts/the-ordinary-case")

	view, err := s.ArticleForUser(ctx, userID, first)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if view.Article.Title != "The ordinary case" {
		t.Errorf("title = %q", view.Article.Title)
	}
	if !view.Read || !view.Starred {
		t.Errorf("read/starred = %v/%v, want both true from the source", view.Read, view.Starred)
	}
	if !view.HasBody {
		t.Error("the imported article has no body")
	}

	content, err := s.CurrentContent(ctx, first)
	if err != nil {
		t.Fatalf("CurrentContent() = %v", err)
	}

	// Immutable, and marked as an import. Both matter: the first is what stops a
	// later extraction replacing what may be the only copy of a dead page, and the
	// second is how an imported body is told apart from one this archive extracted.
	if !content.Immutable {
		t.Error("the imported body is not immutable, so a re-extract could replace it")
	}
	if content.ContentOrigin != store.OriginImport(exchange.SourceWallabag) {
		t.Errorf("content_origin = %q, want import:wallabag", content.ContentOrigin)
	}
	if content.WordCount == 0 {
		t.Error("the imported body has no word count, so search and reading time have nothing")
	}

	// The relative image reference was resolved against the article's own URL,
	// which is what makes it fetchable by the asset pipeline later.
	if !strings.Contains(content.HTML, "https://example.com/relative/figure-2.png") {
		t.Errorf("a relative image was not resolved against the article URL:\n%s", content.HTML)
	}

	// Saved, which is what puts an import in the reading list — and what keeps it
	// out of the retention policy's reach.
	savedAt := countRows(t, ctx, s,
		`SELECT count(*) FROM article_state WHERE user_id = $1 AND saved_at IS NOT NULL`, userID)
	if savedAt != 6 {
		t.Errorf("%d of 6 imported articles are saved, want all of them", savedAt)
	}

	// The source's own saved date, not today's: a ten-year library keeps its
	// chronology.
	var savedYear int
	if err := s.Pool().QueryRow(ctx,
		`SELECT extract(year from saved_at)::int FROM article_state
		 WHERE user_id = $1 AND article_id = $2`, userID, first).Scan(&savedYear); err != nil {
		t.Fatalf("reading saved_at: %v", err)
	}
	if savedYear != 2019 {
		t.Errorf("saved_at year = %d, want 2019 from the source", savedYear)
	}

	// The bodyless records are waiting for this archive's own fetch, which is the
	// improvement an import makes on the library it came from.
	pending := countRows(t, ctx, s, `
		SELECT count(*) FROM articles a
		WHERE a.fetch_status = 'pending'
		  AND NOT EXISTS (SELECT 1 FROM article_content c WHERE c.article_id = a.id)`)
	if pending != 2 {
		t.Errorf("%d bodyless articles are queued for fetching, want 2", pending)
	}

	// Highlights arrive with their text, which is the only thing two systems can
	// agree on about a highlight.
	highlights, err := s.HighlightsForArticle(ctx, userID, first)
	if err != nil {
		t.Fatalf("HighlightsForArticle() = %v", err)
	}
	if len(highlights) != 1 || !strings.HasPrefix(highlights[0].Quote, "The best time") {
		t.Errorf("highlights = %+v", highlights)
	}
	if highlights[0].Note != "This is the whole argument." {
		t.Errorf("highlight note = %q", highlights[0].Note)
	}
}

// Markup from another system goes through the same sanitizer as everything else.
//
// The archive renders stored bodies as trusted HTML on the reader's own origin, and
// an imported body is the oldest markup it will ever hold — a decade-old save can
// carry script written before any of the mitigations that make this safe. If an
// import could bypass the extraction ladder's policy, the archive would have one
// door with a lock and one without.
func TestImportSanitizesWhatItStores(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	apply(t, s, userID)

	id := articleAt(t, s, userID, "https://example.com/posts/markup-from-2011")
	content, err := s.CurrentContent(ctx, id)
	if err != nil {
		t.Fatalf("CurrentContent() = %v", err)
	}

	for _, forbidden := range []string{
		"<script", "alert(", "onclick", "<iframe", "javascript:", "denied:data:",
	} {
		if strings.Contains(content.HTML, forbidden) {
			t.Errorf("the stored body still contains %q:\n%s", forbidden, content.HTML)
		}
	}

	// And the article survived: sanitizing is not discarding.
	if !strings.Contains(content.HTML, "Before the interesting part") ||
		!strings.Contains(content.HTML, "After it") {
		t.Errorf("sanitizing took the prose with it:\n%s", content.HTML)
	}

	// An inline raster image survives, because the bytes are the picture: nothing is
	// fetched and nobody is reached. This test used to assert the opposite, with a
	// comment saying it would need updating if the behavior ever became deliberate.
	// It did, at extractor version 4.
	if !strings.Contains(content.HTML, "data:image/gif;base64") {
		t.Errorf("the inline image was stripped:\n%s", content.HTML)
	}
}

// Re-importing the same export changes nothing.
//
// This is the property that makes "run it again" the honest answer to an import
// that stopped halfway, and it is the acceptance criterion the milestone names:
// zero duplicates on a second run.
func TestImportIsIdempotent(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	apply(t, s, userID)

	before := map[string]int64{
		"articles":   countRows(t, ctx, s, `SELECT count(*) FROM articles`),
		"content":    countRows(t, ctx, s, `SELECT count(*) FROM article_content`),
		"state":      countRows(t, ctx, s, `SELECT count(*) FROM article_state`),
		"tags":       countRows(t, ctx, s, `SELECT count(*) FROM tags`),
		"tagged":     countRows(t, ctx, s, `SELECT count(*) FROM article_tags`),
		"highlights": countRows(t, ctx, s, `SELECT count(*) FROM highlights`),
		"records":    countRows(t, ctx, s, `SELECT count(*) FROM import_records`),
	}

	second := apply(t, s, userID)

	if second.Written.Articles != 0 {
		t.Errorf("a second import wrote %d articles, want 0", second.Written.Articles)
	}
	if second.AlreadyImported != 6 {
		t.Errorf("a second import recognized %d of 6 records", second.AlreadyImported)
	}
	if second.Written.Bodies != 0 || second.Written.HighlightsAdded != 0 ||
		second.Written.TagsAdded != 0 {
		t.Errorf("a second import wrote %d bodies, %d highlights, %d tags; want none",
			second.Written.Bodies, second.Written.HighlightsAdded, second.Written.TagsAdded)
	}

	for table, want := range before {
		var sql string
		switch table {
		case "articles":
			sql = `SELECT count(*) FROM articles`
		case "content":
			sql = `SELECT count(*) FROM article_content`
		case "state":
			sql = `SELECT count(*) FROM article_state`
		case "tags":
			sql = `SELECT count(*) FROM tags`
		case "tagged":
			sql = `SELECT count(*) FROM article_tags`
		case "highlights":
			sql = `SELECT count(*) FROM highlights`
		case "records":
			sql = `SELECT count(*) FROM import_records`
		}
		if got := countRows(t, ctx, s, sql); got != want {
			t.Errorf("%s: %d rows after a second import, want %d", table, got, want)
		}
	}
}

// A re-import must not undo anything the reader did here.
//
// The failure this guards against is the one that makes people stop trusting an
// importer: re-running it to pick up new saves, and finding it has reset the state
// of everything it touched the first time. Tags, highlights, read and starred are
// all additive.
func TestReImportPreservesLocalChanges(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	apply(t, s, userID)

	// The record the source has as unread and untagged.
	id := articleAt(t, s, userID, "https://example.org/notes/wallabag-never-got-this")

	// The reader then reads it, stars it, tags it, and highlights something — all
	// here, after the import.
	if _, err := s.SetRead(ctx, userID, id, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}
	if _, err := s.SetStarred(ctx, userID, id, true); err != nil {
		t.Fatalf("SetStarred() = %v", err)
	}
	local, err := s.EnsureTag(ctx, userID, "added-here")
	if err != nil {
		t.Fatalf("EnsureTag() = %v", err)
	}
	if _, err := s.TagArticle(ctx, userID, id, local); err != nil {
		t.Fatalf("TagArticle() = %v", err)
	}
	if _, err := s.AddHighlight(ctx, userID, id, store.ImportHighlight{
		Quote: "a passage the reader marked here",
	}); err != nil {
		t.Fatalf("AddHighlight() = %v", err)
	}

	apply(t, s, userID)

	view, err := s.ArticleForUser(ctx, userID, id)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if !view.Read {
		t.Error("re-importing marked a read article unread again")
	}
	if !view.Starred {
		t.Error("re-importing un-starred an article")
	}

	tags, err := s.TagsForArticle(ctx, userID, id)
	if err != nil {
		t.Fatalf("TagsForArticle() = %v", err)
	}
	found := false
	for _, tag := range tags {
		if tag.Name == "added-here" {
			found = true
		}
	}
	if !found {
		t.Errorf("re-importing removed a locally-added tag; tags are now %+v", tags)
	}

	highlights, err := s.HighlightsForArticle(ctx, userID, id)
	if err != nil {
		t.Fatalf("HighlightsForArticle() = %v", err)
	}
	if len(highlights) != 1 {
		t.Errorf("re-importing changed the highlights: %+v", highlights)
	}
}

// An imported article that a feed already carried is one article, not two.
//
// This is the whole reason the article rather than the feed item is the root
// entity, and an import is where it pays: a page saved by hand years ago and later
// syndicated through a subscription deduplicates to one row, one body, one set of
// images.
func TestImportDeduplicatesAgainstAnArticleAFeedCarried(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	// A feed the reader subscribes to, carrying the same URL the fixture imports.
	feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://example.com/feed.xml", Title: "Example",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	canonical, err := urlcanon.Canonicalize("https://example.com/posts/the-ordinary-case")
	if err != nil {
		t.Fatalf("Canonicalize() = %v", err)
	}
	existing, created, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: canonical, URLOriginal: canonical, Title: "From the feed",
	})
	if err != nil || !created {
		t.Fatalf("UpsertArticle() = %v (created %v)", err, created)
	}
	if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
		FeedID: feedID, ArticleID: existing, GUID: "guid-from-the-feed",
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}

	report := apply(t, s, userID)

	if report.DuplicateURLs != 1 {
		t.Errorf("the report counted %d duplicate URLs, want 1", report.DuplicateURLs)
	}

	// Six records, one of which was already here: six articles, not seven.
	if n := countRows(t, ctx, s, `SELECT count(*) FROM articles`); n != 6 {
		t.Errorf("%d articles after importing over an existing one, want 6", n)
	}

	// And the import landed on the feed's article, giving it the body the feed
	// never fetched.
	if got := articleAt(t, s, userID, canonical); got != existing {
		t.Errorf("the import created article %d instead of using the feed's %d", got, existing)
	}
	content, err := s.CurrentContent(ctx, existing)
	if err != nil {
		t.Fatalf("CurrentContent() = %v", err)
	}
	if content.ContentOrigin != store.OriginImport(exchange.SourceWallabag) {
		t.Errorf("content_origin = %q, want the imported body", content.ContentOrigin)
	}

	// The feed's own title wins over the import's, because whichever reference saw
	// the article first is the one whose metadata has been reviewed.
	article, err := s.GetArticle(ctx, existing)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.Title != "From the feed" {
		t.Errorf("title = %q; an import should not overwrite metadata already there", article.Title)
	}
}
