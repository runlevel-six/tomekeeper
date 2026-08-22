package store_test

import (
	"errors"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// Two readers, one shared page, two extractions of it.
//
// This is M11's whole claim in one test: the household pays for the fetch once,
// and each reader reads their own body over it. Everything else in the milestone
// is machinery for making that true in more places.
//
// The fixture matters as much as the assertion. The article is **shared** — both
// readers subscribe to a feed carrying it — so visibleArticles admits it for both
// and cannot be what makes the test pass. That is the shape three earlier scoping
// tests in this project got wrong: they rode on the shared visibility predicate and
// passed with the clause under test deleted.
func TestTwoReadersCanExtractOnePageDifferently(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	system := s.System()

	bob, err := system.CreateUser(t.Context(), "bob", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}

	articleID := shareOneArticle(t, s, alice, bob)

	// The household's extraction: what both readers see until one of them asks for
	// something else.
	if _, err := s.InsertContent(t.Context(), store.ContentParams{
		ArticleID:        articleID,
		Owner:            store.Household(),
		ExtractorName:    "trafilatura",
		ExtractorVersion: "6",
		ContentOrigin:    store.OriginFetched,
		HTML:             "<p>the household body</p>",
		Text:             "the household body",
		WordCount:        3,
	}); err != nil {
		t.Fatalf("InsertContent(household) = %v", err)
	}

	// Alice's own extraction, as a domain rule of hers would produce.
	if _, err := s.InsertContent(t.Context(), store.ContentParams{
		ArticleID:        articleID,
		Owner:            store.Owned(alice),
		ExtractorName:    "domain_rule",
		ExtractorVersion: "6",
		ContentOrigin:    store.OriginFetched,
		HTML:             "<p>alice's body</p>",
		Text:             "alice's body",
		WordCount:        2,
	}); err != nil {
		t.Fatalf("InsertContent(alice) = %v", err)
	}

	// Alice reads hers.
	if got := articleBodyFor(t, s, alice, articleID); got != "alice's body" {
		t.Errorf("alice reads %q, want her own extraction", got)
	}

	// Bob reads the household's — not Alice's, and not nothing. Both halves matter:
	// leaking Alice's body to Bob is the failure this milestone exists to prevent,
	// and showing Bob no body at all would be the silent-exclusion failure this
	// project keeps rediscovering.
	if got := articleBodyFor(t, s, bob, articleID); got != "the household body" {
		t.Errorf("bob reads %q, want the household extraction", got)
	}

	// One raw page and one article between them — the household half of the line.
	var articles int
	if err := s.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM articles WHERE id = $1`, articleID).Scan(&articles); err != nil {
		t.Fatalf("counting articles: %v", err)
	}
	if articles != 1 {
		t.Errorf("the page is stored %d times, want once", articles)
	}
}

// A reader's extraction must not demote the household's, or one reader writing a
// domain rule would take everybody else's body away.
func TestAReadersExtractionLeavesTheHouseholdsAlone(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)

	articleID := shareOneArticle(t, s, alice)

	if _, err := s.InsertContent(t.Context(), store.ContentParams{
		ArticleID: articleID, Owner: store.Household(),
		ExtractorName: "trafilatura", ExtractorVersion: "6",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>household</p>", Text: "household", WordCount: 1,
	}); err != nil {
		t.Fatalf("InsertContent(household) = %v", err)
	}
	if _, err := s.InsertContent(t.Context(), store.ContentParams{
		ArticleID: articleID, Owner: store.Owned(alice),
		ExtractorName: "domain_rule", ExtractorVersion: "6",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>alice</p>", Text: "alice", WordCount: 1,
	}); err != nil {
		t.Fatalf("InsertContent(alice) = %v", err)
	}

	// The household's row is still current in its own slot.
	current, err := s.CurrentContent(t.Context(), articleID, store.Household())
	if err != nil {
		t.Fatalf("CurrentContent(household) = %v", err)
	}
	if current.Text != "household" {
		t.Errorf("the household body is now %q; a reader's extraction displaced it", current.Text)
	}

	// And each slot holds exactly one current row, which is the invariant the
	// partial unique index carries.
	var currents int
	if err := s.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM article_content WHERE article_id = $1 AND is_current`,
		articleID).Scan(&currents); err != nil {
		t.Fatalf("counting current bodies: %v", err)
	}
	if currents != 2 {
		t.Errorf("%d current bodies, want 2 (the household's and Alice's)", currents)
	}
}

// Replacing a reader's body replaces theirs, not the household's — the same
// separation from the writing side.
func TestReExtractingForOneReaderTouchesOnlyTheirSlot(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)

	articleID := shareOneArticle(t, s, alice)

	for _, c := range []struct {
		owner *store.UserID
		text  string
	}{
		{store.Household(), "household v1"},
		{store.Owned(alice), "alice v1"},
		{store.Owned(alice), "alice v2"},
	} {
		if _, err := s.InsertContent(t.Context(), store.ContentParams{
			ArticleID: articleID, Owner: c.owner,
			ExtractorName: "trafilatura", ExtractorVersion: "6",
			ContentOrigin: store.OriginFetched,
			HTML:          "<p>" + c.text + "</p>", Text: c.text, WordCount: 2,
		}); err != nil {
			t.Fatalf("InsertContent(%q) = %v", c.text, err)
		}
	}

	household, err := s.CurrentContent(t.Context(), articleID, store.Household())
	if err != nil {
		t.Fatalf("CurrentContent(household) = %v", err)
	}
	if household.Text != "household v1" {
		t.Errorf("household body = %q, want it untouched by Alice's re-extraction", household.Text)
	}

	mine, err := s.CurrentContent(t.Context(), articleID, store.Owned(alice))
	if err != nil {
		t.Fatalf("CurrentContent(alice) = %v", err)
	}
	if mine.Text != "alice v2" {
		t.Errorf("alice's body = %q, want her newest", mine.Text)
	}
}

// A reader with no extraction of their own reads the household's. The fallback is
// the reason a household of readers who never write a rule costs nothing.
func TestAReaderWithNoForkReadsTheHouseholds(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)

	articleID := shareOneArticle(t, s, alice)
	if _, err := s.InsertContent(t.Context(), store.ContentParams{
		ArticleID: articleID, Owner: store.Household(),
		ExtractorName: "trafilatura", ExtractorVersion: "6",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>household</p>", Text: "household", WordCount: 1,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}

	if got := articleBodyFor(t, s, alice, articleID); got != "household" {
		t.Errorf("a reader with no fork reads %q, want the household body", got)
	}

	var rows int
	if err := s.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM article_content WHERE article_id = $1`, articleID).Scan(&rows); err != nil {
		t.Fatalf("counting bodies: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d bodies stored for a reader who customized nothing, want 1", rows)
	}
}

// shareOneArticle creates an article and subscribes every given reader to a feed
// carrying it, so the article is visible to all of them.
func shareOneArticle(t *testing.T, s *store.Store, readers ...store.UserID) store.ArticleID {
	t.Helper()

	articleID, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: "https://example.com/shared-story",
		URLOriginal:  "https://example.com/shared-story",
		Title:        "A shared story",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	for i, reader := range readers {
		feedID, _, err := s.UpsertFeed(t.Context(), reader, store.FeedParams{
			// A different URL per reader, because feeds are unique per
			// (user, feed_url) and each reader has their own subscription to what
			// may well be the same source.
			FeedURL: "https://example.com/feed" + string(rune('a'+i)) + ".xml",
			Title:   "Example",
		})
		if err != nil {
			t.Fatalf("UpsertFeed() = %v", err)
		}
		if _, err := s.InsertFeedItem(t.Context(), reader, store.FeedItemParams{
			FeedID: feedID, ArticleID: articleID, GUID: "shared-story", Title: "A shared story",
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}
	}
	return articleID
}

// articleBodyFor reads one article the way the reader's article page does.
func articleBodyFor(t *testing.T, s *store.Store, reader store.UserID, id store.ArticleID) string {
	t.Helper()

	view, err := s.ArticleForUser(t.Context(), reader, id)
	if err != nil {
		t.Fatalf("ArticleForUser(%d) = %v", reader, err)
	}
	return view.Content.Text
}

// Promoting decides for the reader who asked and for nobody else.
//
// This is the question the design turned on: with one body per article, one
// person choosing which stored copy is shown changed what everyone read — and
// could strand another reader's highlights, which anchor by quoted text rather
// than by body id. Promotion now copies into the asker's slot.
func TestPromotingChoosesForOneReaderOnly(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)

	bob, err := s.System().CreateUser(t.Context(), "bob", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}
	articleID := shareOneArticle(t, s, alice, bob)

	// An imported copy, which wins automatically and is what both readers see.
	if _, err := s.InsertContent(t.Context(), store.ContentParams{
		ArticleID: articleID, Owner: store.Household(),
		ExtractorName: "imported", ExtractorVersion: "1",
		ContentOrigin: store.OriginImport("wallabag"), Immutable: true,
		HTML: "<p>the imported copy</p>", Text: "the imported copy", WordCount: 3,
	}); err != nil {
		t.Fatalf("InsertContent(imported) = %v", err)
	}
	// A later fetch, stored beside it rather than over it.
	if _, err := s.InsertContent(t.Context(), store.ContentParams{
		ArticleID: articleID, Owner: store.Household(),
		ExtractorName: "trafilatura", ExtractorVersion: "6",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>the fetched copy</p>", Text: "the fetched copy", WordCount: 3,
	}); err != nil {
		t.Fatalf("InsertContent(fetched) = %v", err)
	}

	bodies, err := s.BodiesForArticle(t.Context(), alice, articleID)
	if err != nil {
		t.Fatalf("BodiesForArticle() = %v", err)
	}
	var fetched store.ContentID
	for _, b := range bodies {
		if b.ContentOrigin == store.OriginFetched {
			fetched = b.ID
		}
	}
	if fetched == 0 {
		t.Fatal("the fetched body is not among those offered")
	}

	if err := s.PromoteBody(t.Context(), alice, articleID, fetched); err != nil {
		t.Fatalf("PromoteBody() = %v", err)
	}

	if got := articleBodyFor(t, s, alice, articleID); got != "the fetched copy" {
		t.Errorf("alice reads %q after promoting, want the copy she chose", got)
	}
	// The whole point: Bob is unaffected.
	if got := articleBodyFor(t, s, bob, articleID); got != "the imported copy" {
		t.Errorf("bob reads %q, want what he read before alice promoted anything", got)
	}
}

// A reader may not choose a body belonging to another reader, and is told the same
// thing as for a body that does not exist.
func TestAReaderCannotPromoteAnothersBody(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)

	bob, err := s.System().CreateUser(t.Context(), "bob", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}
	articleID := shareOneArticle(t, s, alice, bob)

	if _, err := s.InsertContent(t.Context(), store.ContentParams{
		ArticleID: articleID, Owner: store.Household(),
		ExtractorName: "trafilatura", ExtractorVersion: "6",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>household</p>", Text: "household", WordCount: 1,
	}); err != nil {
		t.Fatalf("InsertContent(household) = %v", err)
	}
	if _, err := s.InsertContent(t.Context(), store.ContentParams{
		ArticleID: articleID, Owner: store.Owned(bob),
		ExtractorName: "domain_rule", ExtractorVersion: "6",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>bob's own</p>", Text: "bob's own", WordCount: 2,
	}); err != nil {
		t.Fatalf("InsertContent(bob) = %v", err)
	}

	var bobsBody store.ContentID
	if err := s.Pool().QueryRow(t.Context(),
		`SELECT id FROM article_content WHERE article_id = $1 AND user_id = $2`,
		articleID, bob).Scan(&bobsBody); err != nil {
		t.Fatalf("finding bob's body: %v", err)
	}

	// Alice may see the article — they share it — so visibility is not what refuses
	// this, which is what makes the assertion worth making.
	if err := s.PromoteBody(t.Context(), alice, articleID, bobsBody); !errors.Is(err, store.ErrNoSuchBody) {
		t.Errorf("promoting another reader's body = %v, want ErrNoSuchBody", err)
	}
	if got := articleBodyFor(t, s, alice, articleID); got != "household" {
		t.Errorf("alice reads %q after a refused promotion, want the household body", got)
	}

	// And the chooser never offered it in the first place.
	bodies, err := s.BodiesForArticle(t.Context(), alice, articleID)
	if err != nil {
		t.Fatalf("BodiesForArticle() = %v", err)
	}
	for _, b := range bodies {
		if b.ID == bobsBody {
			t.Error("the chooser offers another reader's body, which also says they have one")
		}
	}
}
