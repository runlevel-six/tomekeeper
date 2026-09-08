package store_test

import (
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// archiveImage stores one image, links it to an article, and makes it the lead of
// one reader's body — the three things localization does, in the order it does
// them. Returns the store-relative path a list would show it at.
func archiveImage(t *testing.T, s *store.Store, id store.ArticleID, owner *store.UserID,
	sha, html string,
) string {
	t.Helper()

	fsPath := "assets/" + sha[:2] + "/" + sha[2:4] + "/" + sha + ".avif"
	if _, err := s.UpsertAsset(t.Context(), store.Asset{
		SHA256: sha, MediaType: "image/avif", ByteSize: 44_000,
		Width: 1200, Height: 800, FSPath: fsPath,
		SourceURL: "https://example.com/" + sha + ".jpg",
	}); err != nil {
		t.Fatalf("UpsertAsset(%s) = %v", sha, err)
	}
	if err := s.LinkAsset(t.Context(), id, sha); err != nil {
		t.Fatalf("LinkAsset(%s) = %v", sha, err)
	}
	if err := s.UpdateContentHTML(t.Context(), id, owner, html, sha); err != nil {
		t.Fatalf("UpdateContentHTML(%s) = %v", sha, err)
	}
	return fsPath
}

// rowFor finds one article in a reader's stream.
func rowFor(t *testing.T, s *store.Store, userID store.UserID, id store.ArticleID) store.StreamItem {
	t.Helper()

	items, err := s.Stream(t.Context(), userID, store.StreamQuery{})
	if err != nil {
		t.Fatalf("Stream() = %v", err)
	}
	for _, it := range items {
		if it.ArticleID == id {
			return it
		}
	}
	t.Fatalf("article %d is not in the stream of user %d", id, userID)
	return store.StreamItem{}
}

// A row carries the picture its own body leads with, and nothing when there is
// none.
func TestStreamRowCarriesItsLeadImage(t *testing.T) {
	tr := setupTwoReaders(t)

	path := archiveImage(t, tr.store, tr.shared, store.Household(), lead,
		`<p>A story.</p><img src="/assets/`+lead[:2]+`/`+lead[2:4]+`/`+lead+`.avif">`)

	if got := rowFor(t, tr.store, tr.alice, tr.shared).ThumbnailPath; got != path {
		t.Errorf("ThumbnailPath = %q, want %q", got, path)
	}

	// The article next to it has images neither stored nor referenced, and a list
	// has to say so rather than reaching for something.
	if got := rowFor(t, tr.store, tr.alice, tr.aliceOnly).ThumbnailPath; got != "" {
		t.Errorf("an article with no stored image has ThumbnailPath = %q, want empty", got)
	}
}

// A reader sees the lead image of the body *they* read.
//
// The article is deliberately one both readers hold, so the visibility predicate
// cannot be what separates them: if the thumbnail were chosen from the article's
// images rather than through the body, both would see the same picture and this
// would pass while being wrong. Two extractions of one page legitimately lead with
// different pictures — that is the whole point of a reader having their own rule.
func TestAReaderSeesTheLeadImageOfTheirOwnBody(t *testing.T) {
	tr := setupTwoReaders(t)

	household := archiveImage(t, tr.store, tr.shared, store.Household(), lead,
		`<p>Shared Story</p><img src="/assets/x/`+lead+`.avif">`)

	// Alice's own extraction of the same page, leading with a different picture.
	insertBody(t, tr.store, tr.shared, store.ContentParams{
		ExtractorName: "trafilatura", ExtractorVersion: "7",
		ContentOrigin: store.OriginFetched, Owner: store.Owned(tr.alice),
		HTML: "<p>Shared Story, as Alice extracts it.</p>", Text: "Shared Story, as Alice extracts it.",
	})
	mine := archiveImage(t, tr.store, tr.shared, store.Owned(tr.alice), alt,
		`<p>Shared Story, as Alice extracts it.</p><img src="/assets/x/`+alt+`.avif">`)

	if got := rowFor(t, tr.store, tr.alice, tr.shared).ThumbnailPath; got != mine {
		t.Errorf("Alice sees ThumbnailPath = %q, want her own body's %q", got, mine)
	}
	if got := rowFor(t, tr.store, tr.bob, tr.shared).ThumbnailPath; got != household {
		t.Errorf("Bob sees ThumbnailPath = %q, want the household's %q", got, household)
	}
}

// Search results carry the same picture the reading lists do, through the same
// body.
func TestSearchResultsCarryTheLeadImage(t *testing.T) {
	tr := setupTwoReaders(t)

	path := archiveImage(t, tr.store, tr.shared, store.Household(), lead,
		`<p>Shared Story body text, long enough to be a real article.</p>`+
			`<img src="/assets/x/`+lead+`.avif">`)

	results, err := tr.store.Search().Query(t.Context(), tr.alice, store.SearchQuery{Text: "Shared"})
	if err != nil {
		t.Fatalf("Query() = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("the search found nothing to check")
	}
	for _, r := range results {
		if r.ArticleID != tr.shared {
			continue
		}
		if r.ThumbnailPath != path {
			t.Errorf("ThumbnailPath = %q, want %q", r.ThumbnailPath, path)
		}
		return
	}
	t.Fatal("the shared article is not among the results")
}

// A body cannot go on naming a picture the archive has released.
//
// Nothing does this today — releasing an article deletes its bodies before its
// images — so this is the constraint being asked to hold rather than a path being
// exercised. It is here because the alternative, storing the path in the row, would
// pass every test in this file and leave a broken image on the page the first time
// something released an asset on its own.
func TestReleasingAnImageClearsTheThumbnail(t *testing.T) {
	tr := setupTwoReaders(t)

	archiveImage(t, tr.store, tr.shared, store.Household(), lead,
		`<p>A story.</p><img src="/assets/x/`+lead+`.avif">`)

	if _, err := tr.pool.Exec(t.Context(),
		`DELETE FROM article_assets WHERE sha256 = $1`, lead); err != nil {
		t.Fatalf("unlinking the asset: %v", err)
	}
	if _, err := tr.pool.Exec(t.Context(), `DELETE FROM assets WHERE sha256 = $1`, lead); err != nil {
		t.Fatalf("deleting the asset: %v", err)
	}

	if got := rowFor(t, tr.store, tr.alice, tr.shared).ThumbnailPath; got != "" {
		t.Errorf("ThumbnailPath = %q after the image was released, want empty", got)
	}

	// And the row no longer claims an address, which is the half the join cannot
	// prove: a body still naming a deleted image would render the same empty list
	// while holding a reference to nothing.
	var sha *string
	if err := tr.pool.QueryRow(t.Context(),
		`SELECT lead_image_sha FROM article_content WHERE article_id = $1 AND is_current`,
		tr.shared).Scan(&sha); err != nil {
		t.Fatalf("reading the lead image back: %v", err)
	}
	if sha != nil {
		t.Errorf("lead_image_sha = %q after the image was released, want NULL", *sha)
	}
}

// Re-localizing a body that no longer leads with a picture clears the one it had.
//
// The lead is written by the same statement as the HTML precisely so the two cannot
// disagree; this is that promise, from the direction that would otherwise leave a
// thumbnail behind for a picture the article no longer shows.
func TestALaterBodyWithNoImageClearsTheThumbnail(t *testing.T) {
	tr := setupTwoReaders(t)

	archiveImage(t, tr.store, tr.shared, store.Household(), lead,
		`<p>A story.</p><img src="/assets/x/`+lead+`.avif">`)

	if err := tr.store.UpdateContentHTML(t.Context(), tr.shared, store.Household(),
		"<p>A story, with the picture gone.</p>", ""); err != nil {
		t.Fatalf("UpdateContentHTML() = %v", err)
	}

	if got := rowFor(t, tr.store, tr.alice, tr.shared).ThumbnailPath; got != "" {
		t.Errorf("ThumbnailPath = %q, want empty once the body leads with nothing", got)
	}
}

// Two content addresses, long enough to be plausible and distinguishable at a
// glance in a failure message.
const (
	lead = "1eadaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	alt  = "a17aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)
