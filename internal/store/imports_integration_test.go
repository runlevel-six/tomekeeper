package store_test

import (
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// importOf is one record, ready to hand to ImportArticle.
func importOf(slug, body string) store.ImportParams {
	saved := time.Date(2019, 3, 4, 9, 15, 0, 0, time.UTC)
	return store.ImportParams{
		SourceName:   "wallabag",
		SourceID:     slug,
		URLCanonical: "https://example.com/" + slug,
		URLOriginal:  "https://example.com/" + slug,
		Title:        slug,
		SavedAt:      &saved,
		ContentHTML:  body,
		ContentText:  body,
		WordCount:    len(body) / 5,
	}
}

// An imported body is never released by the retention policy.
//
// Retention exists because a body can be fetched again. For an import that premise
// does not hold: it may be the only surviving copy of a page that has since gone,
// so releasing it is not reclaiming space, it is losing the article. Two independent
// things stop it — the body is immutable, and an import is saved, which is a claim —
// and this asserts the first, because the second is a different column set by a
// different code path and a future change could quietly remove it.
//
// Neutering the immutable clause in ExpirableArticles must fail this test.
func TestRetentionNeverReleasesAnImportedBody(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	imported, err := s.ImportArticle(ctx, userID, importOf("the-only-copy",
		"<p>A page that no longer exists anywhere else.</p>"))
	if err != nil {
		t.Fatalf("ImportArticle() = %v", err)
	}

	// Everything retention asks for: read, long ago, not starred, not kept. The one
	// thing an import always carries — saved_at — is cleared here on purpose, so
	// that this test measures the immutable guard rather than the claim check.
	if _, err := s.SetRead(ctx, userID, imported.ArticleID, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}
	longAgo := time.Now().Add(-90 * 24 * time.Hour)
	if _, err := s.Pool().Exec(ctx, `
		UPDATE article_state SET saved_at = NULL, read_at = $3
		WHERE user_id = $1 AND article_id = $2`, userID, imported.ArticleID, longAgo); err != nil {
		t.Fatalf("aging the state row: %v", err)
	}

	expirable, err := s.ExpirableArticles(ctx, time.Now(), 100)
	if err != nil {
		t.Fatalf("ExpirableArticles() = %v", err)
	}
	for _, e := range expirable {
		if e.ArticleID == imported.ArticleID {
			t.Fatalf("the imported body of article %d is expirable; retention would delete "+
				"the only copy of %s", e.ArticleID, e.URL)
		}
	}

	// And a fetched body in the same state *is* expirable, so the test above is the
	// immutable flag doing the work rather than the fixture being unreachable.
	//
	// Saved rather than upserted directly, because an article with no feed reference
	// and no state row is not visible to anyone — the access predicate is what makes
	// it so — and SetRead would silently write nothing against it. A manual save is
	// how such an article legitimately comes to exist.
	saved, err := s.SaveArticle(ctx, userID, "https://example.com/ordinary")
	if err != nil {
		t.Fatalf("SaveArticle() = %v", err)
	}
	ordinary := saved.ArticleID
	if _, err := s.InsertContent(ctx, store.ContentParams{
		ArticleID: ordinary, ExtractorName: "trafilatura", ExtractorVersion: "3",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>A page that can be fetched again.</p>", Text: "A page", WordCount: 5,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}
	if _, err := s.SetRead(ctx, userID, ordinary, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}
	if _, err := s.Pool().Exec(ctx, `
		UPDATE article_state SET saved_at = NULL, read_at = $3
		WHERE user_id = $1 AND article_id = $2`, userID, ordinary, longAgo); err != nil {
		t.Fatalf("aging the state row: %v", err)
	}

	expirable, err = s.ExpirableArticles(ctx, time.Now(), 100)
	if err != nil {
		t.Fatalf("ExpirableArticles() = %v", err)
	}
	found := false
	for _, e := range expirable {
		if e.ArticleID == ordinary {
			found = true
		}
	}
	if !found {
		t.Error("an ordinary read-and-aged article is not expirable, so this test proves nothing")
	}
}

// One imported body per article per source, however many times the import runs.
func TestImportArticleStoresOneBodyPerSource(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	first, err := s.ImportArticle(ctx, userID, importOf("once", "<p>The body.</p>"))
	if err != nil {
		t.Fatalf("ImportArticle() = %v", err)
	}
	if !first.BodyStored {
		t.Error("the first import stored no body")
	}

	second, err := s.ImportArticle(ctx, userID, importOf("once", "<p>The body.</p>"))
	if err != nil {
		t.Fatalf("ImportArticle() twice = %v", err)
	}
	if second.BodyStored {
		t.Error("a second import stored the body again")
	}
	if !second.AlreadyImported {
		t.Error("a second import did not recognize the record")
	}

	var bodies int64
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM article_content WHERE article_id = $1`, first.ArticleID).Scan(&bodies); err != nil {
		t.Fatalf("counting bodies: %v", err)
	}
	if bodies != 1 {
		t.Errorf("article %d has %d bodies, want 1", first.ArticleID, bodies)
	}
}

// A record with no stable source id still imports, and deduplicates by URL.
//
// Not every source has an id worth keying on. The fallback is the canonical URL,
// which is where identity lives anyway — so the article is not duplicated, even
// though the import cannot be recognized by id on a later run.
func TestImportArticleWithoutASourceID(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	p := importOf("no-id", "<p>A record with no id.</p>")
	p.SourceID = ""

	first, err := s.ImportArticle(ctx, userID, p)
	if err != nil {
		t.Fatalf("ImportArticle() = %v", err)
	}
	second, err := s.ImportArticle(ctx, userID, p)
	if err != nil {
		t.Fatalf("ImportArticle() twice = %v", err)
	}

	if second.ArticleID != first.ArticleID {
		t.Errorf("a second import created article %d instead of reusing %d",
			second.ArticleID, first.ArticleID)
	}
	if second.AlreadyImported {
		t.Error("a record with no id was reported as recognized, which it cannot be")
	}

	var records int64
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM import_records WHERE user_id = $1`, userID).Scan(&records); err != nil {
		t.Fatalf("counting import records: %v", err)
	}
	if records != 0 {
		t.Errorf("%d import records were written for a record with no id, want 0", records)
	}
}

// An import is one reader's, and so is everything it carries.
//
// Built on a *shared* article, which is the only shape that tests anything: both
// readers import the same URL, so it is legitimately visible to both, and what must
// not cross is the state, the highlights, and the import bookkeeping. An article
// only one of them could see would pass with every user scope in this file deleted.
func TestImportsAreScopedToOneReader(t *testing.T) {
	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	var bob store.UserID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("creating the second user: %v", err)
	}

	shared := importOf("shared-page", "<p>A page they both saved.</p>")

	hers, err := s.ImportArticle(ctx, alice, shared)
	if err != nil {
		t.Fatalf("ImportArticle(alice) = %v", err)
	}

	// Alice reads it and highlights something.
	if _, err := s.SetRead(ctx, alice, hers.ArticleID, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}
	if _, err := s.AddHighlight(ctx, alice, hers.ArticleID, store.ImportHighlight{
		Quote: "a passage only Alice marked",
	}); err != nil {
		t.Fatalf("AddHighlight() = %v", err)
	}

	// Bob imports the same library.
	his, err := s.ImportArticle(ctx, bob, shared)
	if err != nil {
		t.Fatalf("ImportArticle(bob) = %v", err)
	}

	// One article, because it is one page.
	if his.ArticleID != hers.ArticleID {
		t.Fatalf("the same URL imported as two articles: %d and %d", hers.ArticleID, his.ArticleID)
	}
	// But not "already imported": the record is Alice's, and Bob's import is his own.
	if his.AlreadyImported {
		t.Error("Bob's import was treated as already done because Alice had done hers")
	}

	// Alice's reading is not Bob's.
	view, err := s.ArticleForUser(ctx, bob, his.ArticleID)
	if err != nil {
		t.Fatalf("ArticleForUser(bob) = %v", err)
	}
	if view.Read {
		t.Error("Bob's copy is read because Alice read hers")
	}

	// Nor are her highlights.
	bobsHighlights, err := s.HighlightsForArticle(ctx, bob, his.ArticleID)
	if err != nil {
		t.Fatalf("HighlightsForArticle(bob) = %v", err)
	}
	if len(bobsHighlights) != 0 {
		t.Errorf("Bob can see %d of Alice's highlights: %+v", len(bobsHighlights), bobsHighlights)
	}

	// And an import record is per reader, so each of them can re-run their own
	// import and have it recognized.
	for _, who := range []struct {
		name string
		id   store.UserID
	}{{"alice", alice}, {"bob", bob}} {
		_, found, err := s.ImportedArticle(ctx, who.id, "wallabag", "shared-page")
		if err != nil {
			t.Fatalf("ImportedArticle(%s) = %v", who.name, err)
		}
		if !found {
			t.Errorf("%s has no import record for a record she or he imported", who.name)
		}
	}
}

// The report's duplicate count asks about the reader's own archive, not everyone's.
//
// GetArticleByURL answers a question about the shared article pool, which is right
// for the fetch pipeline and wrong here: a report that counted another reader's
// saves as duplicates would be both inaccurate and one reader learning what another
// has. Deleting the scope from ArticleVisibleByURL must fail this test.
func TestArticleVisibleByURLIsScopedToTheReader(t *testing.T) {
	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	var bob store.UserID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("creating the second user: %v", err)
	}

	his, err := s.ImportArticle(ctx, bob, importOf("bobs-page", "<p>Bob's page.</p>"))
	if err != nil {
		t.Fatalf("ImportArticle(bob) = %v", err)
	}

	// The article exists, and the shared-pool lookup finds it.
	if _, err := s.GetArticleByURL(ctx, "https://example.com/bobs-page"); err != nil {
		t.Fatalf("GetArticleByURL() = %v; the fixture is wrong", err)
	}

	// Alice's archive does not contain it.
	id, found, err := s.ArticleVisibleByURL(ctx, alice, "https://example.com/bobs-page")
	if err != nil {
		t.Fatalf("ArticleVisibleByURL(alice) = %v", err)
	}
	if found {
		t.Errorf("Alice's archive reports article %d, which is Bob's", id)
	}

	// Bob's does.
	id, found, err = s.ArticleVisibleByURL(ctx, bob, "https://example.com/bobs-page")
	if err != nil {
		t.Fatalf("ArticleVisibleByURL(bob) = %v", err)
	}
	if !found || id != his.ArticleID {
		t.Errorf("Bob's archive reports %d/%v, want %d", id, found, his.ArticleID)
	}
}

// A highlight cannot be written against an article the reader cannot see.
//
// The same rule as every other state write: allowing it would let one reader
// confirm what another has archived, one insert at a time.
func TestAddHighlightRefusesAnInvisibleArticle(t *testing.T) {
	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	var bob store.UserID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("creating the second user: %v", err)
	}

	his, err := s.ImportArticle(ctx, bob, importOf("bobs-other-page", "<p>Bob's page.</p>"))
	if err != nil {
		t.Fatalf("ImportArticle(bob) = %v", err)
	}

	written, err := s.AddHighlight(ctx, alice, his.ArticleID, store.ImportHighlight{
		Quote: "a passage in an article Alice cannot see",
	})
	if err != nil {
		t.Fatalf("AddHighlight() = %v", err)
	}
	if written {
		t.Error("Alice highlighted an article she cannot see")
	}

	var rows int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM highlights WHERE user_id = $1`, alice).Scan(&rows); err != nil {
		t.Fatalf("counting highlights: %v", err)
	}
	if rows != 0 {
		t.Errorf("Alice has %d highlights, want 0", rows)
	}
}

// Highlights are matched on their text, so a re-import does not stack copies.
func TestAddHighlightDeduplicatesOnTheQuote(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	imported, err := s.ImportArticle(ctx, userID, importOf("with-a-highlight", "<p>A body.</p>"))
	if err != nil {
		t.Fatalf("ImportArticle() = %v", err)
	}

	h := store.ImportHighlight{Quote: "the same passage", Note: "the first note"}

	first, err := s.AddHighlight(ctx, userID, imported.ArticleID, h)
	if err != nil {
		t.Fatalf("AddHighlight() = %v", err)
	}
	second, err := s.AddHighlight(ctx, userID, imported.ArticleID, h)
	if err != nil {
		t.Fatalf("AddHighlight() twice = %v", err)
	}
	if !first || second {
		t.Errorf("AddHighlight wrote %v then %v, want true then false", first, second)
	}

	highlights, err := s.HighlightsForArticle(ctx, userID, imported.ArticleID)
	if err != nil {
		t.Fatalf("HighlightsForArticle() = %v", err)
	}
	if len(highlights) != 1 {
		t.Errorf("the article has %d highlights, want 1", len(highlights))
	}
}

// ImportCounts is what an operator asks to find out whether an import happened.
func TestImportCounts(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	for _, slug := range []string{"one", "two", "three"} {
		if _, err := s.ImportArticle(ctx, userID, importOf(slug, "<p>A body.</p>")); err != nil {
			t.Fatalf("ImportArticle(%s) = %v", slug, err)
		}
	}

	counts, err := s.ImportCounts(ctx, userID)
	if err != nil {
		t.Fatalf("ImportCounts() = %v", err)
	}
	if len(counts) != 1 || counts[0].SourceName != "wallabag" || counts[0].Articles != 3 {
		t.Errorf("ImportCounts() = %+v, want 3 from wallabag", counts)
	}
}
