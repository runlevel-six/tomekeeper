package store_test

import (
	"fmt"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The audit lenses, scoped to one reader.
//
// The three queries behind `tome audit` are archive-wide, which is right for an
// operator maintaining an archive and wrong for a page. These tests are about the
// two narrowings that turn one into the other: a reader sees findings only about
// articles they can see, and a finding is about the body *they* read — their own
// extraction where their rules produced one, the household's otherwise.
//
// Every case below puts the interesting body on an article **both** readers can see,
// so that what separates the two answers is ownership and nothing else. A test built
// on an article only one of them can reach would pass with the body predicate deleted
// entirely, which is the shape of vacuous test this suite has been bitten by before.

// A title with four words no wall would ever mention, and two bodies: one that is
// plainly the article, one that is plainly a consent gate.
const (
	consensusTitle = "Understanding Distributed Consensus Algorithms"

	consensusBody = "Understanding distributed consensus algorithms takes practice, and this piece " +
		"works through several worked examples before arriving at the interesting part."

	consentWall = "This website uses cookies to improve your experience. Please accept or " +
		"decline before continuing, and review our privacy notice for the full detail."
)

// A reader's own fork is the body judged for them, and the household's is judged for
// everybody else.
func TestSuspectBodiesJudgeTheBodyTheReaderReads(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	shared := auditArticle(t, tr, "https://example.com/consensus", consensusTitle)
	auditVisible(t, tr, shared, tr.alice)
	auditVisible(t, tr, shared, tr.bob)

	// The household's extraction is the article, and it is what Bob reads.
	auditBody(t, tr, shared, store.Household(), consensusBody)
	// Alice's own rule found the consent gate instead. Copy-on-write: this is a
	// second current row on the same article, not a replacement.
	auditBody(t, tr, shared, store.Owned(tr.alice), consentWall)

	if got := auditSuspectIDs(t, tr, tr.alice); !got[shared] {
		t.Error("Alice's own body is a consent wall and the lens did not flag it")
	}
	if got := auditSuspectIDs(t, tr, tr.bob); got[shared] {
		t.Error("Bob reads the household's body, which is the article; flagging it for him " +
			"means the lens judged somebody else's extraction")
	}

	// And the operator's form reports the bad body once, not once per current body on
	// the article. Grouping by article rather than by body was the pre-tenancy shape:
	// it counted the title's words twice, let the good body's overlap clear the bad
	// one, and emitted duplicate rows.
	all, err := tr.store.SuspectBodies(ctx, 50)
	if err != nil {
		t.Fatalf("SuspectBodies() = %v", err)
	}
	found := 0
	for _, b := range all {
		if b.ArticleID == shared {
			found++
		}
	}
	if found != 1 {
		t.Errorf("the archive-wide lens reported the article %d times, want exactly 1: %+v", found, all)
	}
}

// Nothing about another reader's articles reaches any of the three lenses.
func TestAuditLensesHideAnotherReadersArticles(t *testing.T) {
	tr := setupTwoReaders(t)

	// Bob's alone, and wrong in two different ways: a body that is a consent wall,
	// and a title that is a URL.
	bobsWall := auditArticle(t, tr, "https://example.com/bobs-consensus", consensusTitle)
	auditVisible(t, tr, bobsWall, tr.bob)
	auditBody(t, tr, bobsWall, store.Household(), consentWall)

	bobsTitle := auditArticle(t, tr, "https://example.com/bobs-bookmark",
		"https://example.com/bobs-bookmark")
	auditVisible(t, tr, bobsTitle, tr.bob)
	auditBody(t, tr, bobsTitle, store.Household(), consensusBody)

	if got := auditSuspectIDs(t, tr, tr.bob); !got[bobsWall] {
		t.Fatal("Bob cannot see a finding about his own article, so this test proves nothing")
	}
	if got := auditSuspectIDs(t, tr, tr.alice); got[bobsWall] {
		t.Error("Alice was shown a finding about an article she cannot see, which tells her it exists")
	}

	titles, err := tr.store.PlaceholderTitlesFor(t.Context(), tr.alice, 50)
	if err != nil {
		t.Fatalf("PlaceholderTitlesFor(alice) = %v", err)
	}
	for _, title := range titles {
		if title.ArticleID == bobsTitle {
			t.Error("Alice was shown Bob's URL-titled article")
		}
	}

	bobTitles, err := tr.store.PlaceholderTitlesFor(t.Context(), tr.bob, 50)
	if err != nil {
		t.Fatalf("PlaceholderTitlesFor(bob) = %v", err)
	}
	seen := false
	for _, title := range bobTitles {
		if title.ArticleID == bobsTitle {
			seen = true
			if !title.HasBody {
				t.Error("the article has a household body, so it wants re-extraction rather than a fetch")
			}
		}
	}
	if !seen {
		t.Error("Bob is not shown his own URL-titled article")
	}
}

// A shared body is a finding only when the reader can see both articles sharing it.
func TestSharedBodiesNeedsBothHalvesVisible(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	// The same wall stored as the article of two different pages: the finding this
	// lens exists for. One of the two is Bob's alone.
	both := auditArticle(t, tr, "https://example.com/wall-one", consensusTitle)
	auditVisible(t, tr, both, tr.alice)
	auditVisible(t, tr, both, tr.bob)
	auditBody(t, tr, both, store.Household(), consentWall)

	bobsOnly := auditArticle(t, tr, "https://example.com/wall-two", consensusTitle)
	auditVisible(t, tr, bobsOnly, tr.bob)
	auditBody(t, tr, bobsOnly, store.Household(), consentWall)

	bobPairs, err := tr.store.SharedBodiesFor(ctx, tr.bob, 50)
	if err != nil {
		t.Fatalf("SharedBodiesFor(bob) = %v", err)
	}
	if len(bobPairs) != 1 || len(bobPairs[0].ArticleIDs) != 2 {
		t.Fatalf("Bob can see both halves and was shown %+v", bobPairs)
	}

	alicePairs, err := tr.store.SharedBodiesFor(ctx, tr.alice, 50)
	if err != nil {
		t.Fatalf("SharedBodiesFor(alice) = %v", err)
	}
	if len(alicePairs) != 0 {
		// Not merely noise: the pairing is itself the leak. Telling Alice that this
		// body is shared with something says there is another article she cannot see.
		t.Errorf("Alice can see one of the two articles and was shown the pair: %+v", alicePairs)
	}
}

// One article's two current bodies are not "a body more than one article shares".
func TestSharedBodiesIgnoresOneArticlesOwnFork(t *testing.T) {
	tr := setupTwoReaders(t)

	// A rule of Alice's that happens to select exactly what the household's
	// extraction already produced. Byte-identical, and the same article — so counting
	// rows rather than distinct articles reported this as a shared body, which is a
	// finding about nothing.
	forked := auditArticle(t, tr, "https://example.com/identical", consensusTitle)
	auditVisible(t, tr, forked, tr.alice)
	auditBody(t, tr, forked, store.Household(), consensusBody)
	auditBody(t, tr, forked, store.Owned(tr.alice), consensusBody)

	all, err := tr.store.SharedBodies(t.Context(), 50)
	if err != nil {
		t.Fatalf("SharedBodies() = %v", err)
	}
	for _, pair := range all {
		for _, id := range pair.ArticleIDs {
			if id == forked {
				t.Errorf("an article was reported as sharing a body with itself: %+v", pair)
			}
		}
	}
}

// "Has a body" is asked of the reader asking, because the remedy differs.
//
// An article with a body has a stored page with a real title in it and wants
// re-extraction; one without has nothing to read a title out of and wants a fetch.
// Offering the wrong remedy is offering something that cannot work.
func TestPlaceholderTitleBodyFollowsTheAskingReader(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	// No household body at all: the only extraction of this page belongs to Alice.
	bookmark := auditArticle(t, tr, "https://example.com/bookmark",
		"https%3A%2F%2Fexample.com%2Fbookmark")
	auditVisible(t, tr, bookmark, tr.alice)
	auditVisible(t, tr, bookmark, tr.bob)
	auditBody(t, tr, bookmark, store.Owned(tr.alice), consensusBody)

	has := func(userID store.UserID) bool {
		t.Helper()
		titles, err := tr.store.PlaceholderTitlesFor(ctx, userID, 50)
		if err != nil {
			t.Fatalf("PlaceholderTitlesFor() = %v", err)
		}
		for _, title := range titles {
			if title.ArticleID == bookmark {
				return title.HasBody
			}
		}
		t.Fatalf("the URL-titled article was not reported to user %d at all", userID)
		return false
	}

	if !has(tr.alice) {
		t.Error("Alice holds the only body of this page and was told there is none to re-extract")
	}
	if has(tr.bob) {
		t.Error("Bob has no body of this page and was offered re-extraction, which would find nothing; " +
			"he was shown Alice's body")
	}
}

// auditArticle stores an article with the given title.
func auditArticle(t *testing.T, tr twoReaders, url, title string) store.ArticleID {
	t.Helper()

	id, _, err := tr.store.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: url, URLOriginal: url, Title: title,
	})
	if err != nil {
		t.Fatalf("UpsertArticle(%s) = %v", url, err)
	}
	return id
}

// auditVisible makes an article reachable by one reader, through a subscription.
func auditVisible(t *testing.T, tr twoReaders, id store.ArticleID, userID store.UserID) {
	t.Helper()

	feedID := tr.aliceFeed
	if userID == tr.bob {
		feedID = tr.bobFeed
	}
	if _, err := tr.store.InsertFeedItem(t.Context(), userID, store.FeedItemParams{
		FeedID: feedID, ArticleID: id, GUID: fmt.Sprintf("audit-%d-%d", userID, id),
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
}

// auditBody stores a current body in one owner's slot.
func auditBody(t *testing.T, tr twoReaders, id store.ArticleID, owner *store.UserID, text string) {
	t.Helper()

	if _, err := tr.store.InsertContent(t.Context(), store.ContentParams{
		ArticleID: id, Owner: owner,
		ExtractorName: "trafilatura", ExtractorVersion: "7",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>" + text + "</p>", Text: text,
		// Above the twenty-word floor the shared-body lens applies, so a body used in
		// that lens is not discarded as a stub.
		WordCount: 24,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}
}

// auditSuspectIDs is the title lens as a set, for one reader.
func auditSuspectIDs(t *testing.T, tr twoReaders, userID store.UserID) map[store.ArticleID]bool {
	t.Helper()

	bodies, err := tr.store.SuspectBodiesFor(t.Context(), userID, 50)
	if err != nil {
		t.Fatalf("SuspectBodiesFor(%d) = %v", userID, err)
	}
	got := make(map[store.ArticleID]bool, len(bodies))
	for _, b := range bodies {
		got[b.ArticleID] = true
	}
	return got
}
