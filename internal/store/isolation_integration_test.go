package store_test

import (
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// M4's acceptance criterion, and §2.8's whole reason for existing: two users with
// distinct feed sets, and neither can see, modify, or infer the other's articles.
//
// The plan is explicit that this must pass at M4, before any user management
// exists. That is the point — the discipline is cheap to keep now and a rewrite to
// retrofit, so the test is written while there is only one real user.

// twoReaders sets up two users, each subscribed to their own feed with their own
// article, plus one article both feeds carry.
type twoReaders struct {
	store *store.Store

	alice, bob         store.UserID
	aliceFeed, bobFeed store.FeedID

	// aliceOnly and bobOnly are reachable by exactly one reader. shared is carried
	// by both feeds and is one row in articles — the deduplication §2.1 buys.
	aliceOnly, bobOnly, shared store.ArticleID
}

func setupTwoReaders(t *testing.T) twoReaders {
	t.Helper()

	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	// A second user, created directly: M9 owns signup, and this test must not wait
	// for it.
	var bob store.UserID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("creating the second user: %v", err)
	}

	aliceFeed, _, err := s.UpsertFeed(ctx, alice, store.FeedParams{
		FeedURL: "https://alice.example.com/feed.xml", Title: "Alice's Feed",
	})
	if err != nil {
		t.Fatalf("UpsertFeed(alice) = %v", err)
	}
	bobFeed, _, err := s.UpsertFeed(ctx, bob, store.FeedParams{
		FeedURL: "https://bob.example.com/feed.xml", Title: "Bob's Feed",
	})
	if err != nil {
		t.Fatalf("UpsertFeed(bob) = %v", err)
	}

	article := func(url, title string) store.ArticleID {
		t.Helper()
		id, _, err := s.UpsertArticle(ctx, store.ArticleParams{URLCanonical: url, Title: title})
		if err != nil {
			t.Fatalf("UpsertArticle(%s) = %v", url, err)
		}
		insertBody(t, s, id, store.ContentParams{
			ExtractorName:    "trafilatura",
			ExtractorVersion: "2",
			ContentOrigin:    store.OriginFetched,
			HTML:             "<p>" + title + " body text, long enough to be a real article.</p>",
			Text:             title + " body text, long enough to be a real article.",
			WordCount:        10,
		})
		return id
	}

	tr := twoReaders{store: s, alice: alice, bob: bob, aliceFeed: aliceFeed, bobFeed: bobFeed}
	tr.aliceOnly = article("https://example.com/alice-only", "Alice Only")
	tr.bobOnly = article("https://example.com/bob-only", "Bob Only")
	tr.shared = article("https://example.com/shared", "Shared Story")

	link := func(userID store.UserID, feedID store.FeedID, articleID store.ArticleID, guid string) {
		t.Helper()
		if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
			FeedID: feedID, ArticleID: articleID, GUID: guid,
		}); err != nil {
			t.Fatalf("InsertFeedItem(%s) = %v", guid, err)
		}
	}
	link(alice, aliceFeed, tr.aliceOnly, "alice-1")
	link(alice, aliceFeed, tr.shared, "alice-shared")
	link(bob, bobFeed, tr.bobOnly, "bob-1")
	link(bob, bobFeed, tr.shared, "bob-shared")

	return tr
}

func TestStreamShowsOnlyYourOwnArticles(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	ids := func(userID store.UserID) map[store.ArticleID]bool {
		t.Helper()
		items, err := tr.store.Stream(ctx, userID, store.StreamQuery{})
		if err != nil {
			t.Fatalf("Stream() = %v", err)
		}
		got := make(map[store.ArticleID]bool, len(items))
		for _, it := range items {
			got[it.ArticleID] = true
		}
		return got
	}

	alice := ids(tr.alice)
	bob := ids(tr.bob)

	if !alice[tr.aliceOnly] {
		t.Error("Alice cannot see her own article")
	}
	if !alice[tr.shared] {
		t.Error("Alice cannot see the shared article")
	}
	if alice[tr.bobOnly] {
		t.Error("Alice can see Bob's article, which is the leak §2.8 exists to prevent")
	}

	if !bob[tr.bobOnly] || !bob[tr.shared] {
		t.Error("Bob cannot see his own articles")
	}
	if bob[tr.aliceOnly] {
		t.Error("Bob can see Alice's article")
	}
}

// A syndicated story is one row shared by both readers, and it must appear once
// per reader rather than once per feed that carried it.
func TestSharedArticleAppearsOncePerReader(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	// Give Alice a second feed that also carries the shared story.
	second, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL: "https://alice.example.com/other.xml", Title: "Alice's Other Feed",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if _, err := tr.store.InsertFeedItem(ctx, tr.alice, store.FeedItemParams{
		FeedID: second, ArticleID: tr.shared, GUID: "alice-shared-again",
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}

	items, err := tr.store.Stream(ctx, tr.alice, store.StreamQuery{})
	if err != nil {
		t.Fatalf("Stream() = %v", err)
	}

	var seen int
	for _, it := range items {
		if it.ArticleID == tr.shared {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the shared article appears %d times in Alice's stream, want 1", seen)
	}
}

// Reading one reader's copy must not mark the other's.
func TestReadStateIsPerReader(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	ok, err := tr.store.SetRead(ctx, tr.alice, tr.shared, true)
	if err != nil {
		t.Fatalf("SetRead() = %v", err)
	}
	if !ok {
		t.Fatal("SetRead() reported no row written for an article Alice can see")
	}

	aliceView, err := tr.store.ArticleForUser(ctx, tr.alice, tr.shared)
	if err != nil {
		t.Fatalf("ArticleForUser(alice) = %v", err)
	}
	if !aliceView.Read {
		t.Error("Alice's copy is not marked read")
	}

	bobView, err := tr.store.ArticleForUser(ctx, tr.bob, tr.shared)
	if err != nil {
		t.Fatalf("ArticleForUser(bob) = %v", err)
	}
	if bobView.Read {
		t.Error("Bob's copy of the shared article was marked read by Alice reading hers")
	}
}

// Not found rather than forbidden. "Forbidden" would confirm the article exists,
// which §2.8 says one reader must not be able to infer about another's archive.
func TestReadingAnotherReadersArticleIsNotFound(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	_, err := tr.store.ArticleForUser(ctx, tr.alice, tr.bobOnly)
	if err == nil {
		t.Fatal("ArticleForUser() returned Bob's article to Alice")
	}
	if !store.IsNotFound(err) {
		t.Errorf("ArticleForUser() = %v, want a not-found error so existence is not confirmed", err)
	}
}

// Nor may a reader write state against an article they cannot see: allowing it
// would let them confirm what exists one insert at a time.
func TestCannotSetStateOnAnotherReadersArticle(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	ok, err := tr.store.SetRead(ctx, tr.alice, tr.bobOnly, true)
	if err != nil {
		t.Fatalf("SetRead() = %v", err)
	}
	if ok {
		t.Error("Alice wrote read state against Bob's article")
	}

	ok, err = tr.store.SetStarred(ctx, tr.alice, tr.bobOnly, true)
	if err != nil {
		t.Fatalf("SetStarred() = %v", err)
	}
	if ok {
		t.Error("Alice starred Bob's article")
	}

	// And it stayed invisible: a refused write must not have created a state row
	// that makes it visible on the next request.
	if _, err := tr.store.ArticleForUser(ctx, tr.alice, tr.bobOnly); !store.IsNotFound(err) {
		t.Errorf("after refused writes, ArticleForUser() = %v, want not found", err)
	}
}

func TestFeedListsAreSeparate(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	for _, c := range []struct {
		name   string
		userID store.UserID
		want   store.FeedID
		wantNo store.FeedID
	}{
		{"alice", tr.alice, tr.aliceFeed, tr.bobFeed},
		{"bob", tr.bob, tr.bobFeed, tr.aliceFeed},
	} {
		feeds, err := tr.store.ListFeeds(ctx, c.userID)
		if err != nil {
			t.Fatalf("ListFeeds(%s) = %v", c.name, err)
		}
		var sawWanted, sawOther bool
		for _, f := range feeds {
			switch f.ID {
			case c.want:
				sawWanted = true
			case c.wantNo:
				sawOther = true
			}
		}
		if !sawWanted {
			t.Errorf("%s cannot see their own feed", c.name)
		}
		if sawOther {
			t.Errorf("%s can see the other reader's feed", c.name)
		}
	}
}

func TestTagListsAreSeparate(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	// Both readers use the same tag name, which must be two independent tags.
	aliceTag, err := tr.store.EnsureTag(ctx, tr.alice, "distributed systems")
	if err != nil {
		t.Fatalf("EnsureTag(alice) = %v", err)
	}
	bobTag, err := tr.store.EnsureTag(ctx, tr.bob, "distributed systems")
	if err != nil {
		t.Fatalf("EnsureTag(bob) = %v", err)
	}
	if aliceTag == bobTag {
		t.Fatal("both readers were given the same tag row")
	}

	if ok, err := tr.store.TagArticle(ctx, tr.alice, tr.aliceOnly, aliceTag); err != nil || !ok {
		t.Fatalf("TagArticle(alice) = %v, %v", ok, err)
	}

	// Alice's tag must not be usable by Bob, even on an article Bob can see.
	if ok, err := tr.store.TagArticle(ctx, tr.bob, tr.shared, aliceTag); err != nil {
		t.Fatalf("TagArticle(bob, alice's tag) = %v", err)
	} else if ok {
		t.Error("Bob attached Alice's tag to an article")
	}

	aliceTags, err := tr.store.ListTags(ctx, tr.alice)
	if err != nil {
		t.Fatalf("ListTags(alice) = %v", err)
	}
	bobTags, err := tr.store.ListTags(ctx, tr.bob)
	if err != nil {
		t.Fatalf("ListTags(bob) = %v", err)
	}

	if len(aliceTags) != 1 || aliceTags[0].ID != aliceTag {
		t.Errorf("Alice's tag list = %+v, want just her own tag", aliceTags)
	}
	if len(bobTags) != 1 || bobTags[0].ID != bobTag {
		t.Errorf("Bob's tag list = %+v, want just his own tag", bobTags)
	}

	// Counts must reflect only what each reader can see.
	if aliceTags[0].Count != 1 {
		t.Errorf("Alice's tag counts %d articles, want 1", aliceTags[0].Count)
	}
	if bobTags[0].Count != 0 {
		t.Errorf("Bob's unused tag counts %d articles, want 0", bobTags[0].Count)
	}
}

func TestUnreadCountsAreSeparate(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	// Alice reads both of hers.
	for _, id := range []store.ArticleID{tr.aliceOnly, tr.shared} {
		if _, err := tr.store.SetRead(ctx, tr.alice, id, true); err != nil {
			t.Fatalf("SetRead() = %v", err)
		}
	}

	alice, err := tr.store.UnreadCountsFor(ctx, tr.alice)
	if err != nil {
		t.Fatalf("UnreadCountsFor(alice) = %v", err)
	}
	if alice.Total != 0 {
		t.Errorf("Alice has %d unread after reading both, want 0", alice.Total)
	}

	bob, err := tr.store.UnreadCountsFor(ctx, tr.bob)
	if err != nil {
		t.Fatalf("UnreadCountsFor(bob) = %v", err)
	}
	if bob.Total != 2 {
		t.Errorf("Bob has %d unread, want 2 — Alice reading hers must not affect his", bob.Total)
	}
	if got := bob.ByFeed[tr.bobFeed]; got != 2 {
		t.Errorf("Bob's feed shows %d unread, want 2", got)
	}
	if _, present := bob.ByFeed[tr.aliceFeed]; present {
		t.Error("Bob's per-feed counts include Alice's feed")
	}
}

func TestNeedsAttentionIsScoped(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	if err := tr.store.RecordFetchFailure(ctx, tr.bobOnly, store.FetchFailed,
		"extraction produced no content"); err != nil {
		t.Fatalf("RecordFetchFailure() = %v", err)
	}

	alice, err := tr.store.NeedsAttentionFor(ctx, tr.alice, 50)
	if err != nil {
		t.Fatalf("NeedsAttentionFor(alice) = %v", err)
	}
	for _, n := range alice {
		if n.ArticleID == tr.bobOnly {
			t.Error("Bob's failed article appears in Alice's attention queue")
		}
	}

	bob, err := tr.store.NeedsAttentionFor(ctx, tr.bob, 50)
	if err != nil {
		t.Fatalf("NeedsAttentionFor(bob) = %v", err)
	}
	var found bool
	for _, n := range bob {
		if n.ArticleID == tr.bobOnly {
			found = true
			if n.FetchStatus != store.FetchFailed {
				t.Errorf("fetch status = %q, want %q", n.FetchStatus, store.FetchFailed)
			}
			if n.FetchError == "" {
				t.Error("the reason is missing, which is the only useful part of the queue")
			}
		}
	}
	if !found {
		t.Error("Bob's failed article is missing from his own attention queue")
	}
}
