package store_test

import (
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// seedCategorized subscribes one user to a feed in a named category and puts one
// article on it, returning the article.
//
// The category is the feed's own column rather than a table, so "seeding a
// category" is really seeding a feed that claims one.
func seedCategorized(t *testing.T, s *store.Store, userID store.UserID,
	category, feedTitle, slug string, published time.Time,
) store.ArticleID {
	t.Helper()
	ctx := t.Context()

	feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL:  "https://example.com/" + slug + "/feed.xml",
		Title:    feedTitle,
		Category: category,
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	id, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://example.com/" + slug,
		Title:        feedTitle + " post",
		PublishedAt:  &published,
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	insertBody(t, s, id, store.ContentParams{Text: "body of " + slug})

	if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
		FeedID: feedID, ArticleID: id, GUID: "guid-" + slug,
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
	return id
}

func TestListCategoriesGroupsFeedsAndCountsUnread(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	comic := seedCategorized(t, s, userID, "Comics", "xkcd", "xkcd", base)
	seedCategorized(t, s, userID, "Comics", "MonkeyUser", "monkeyuser", base.Add(time.Minute))
	seedCategorized(t, s, userID, "Tech", "Fowler", "fowler", base.Add(2*time.Minute))
	// No category at all: an OPML file may list a feed outside every folder.
	seedCategorized(t, s, userID, "", "Loose", "loose", base.Add(3*time.Minute))

	// One read article, so the unread count is not merely the article count.
	if _, err := s.SetRead(ctx, userID, comic, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}

	got, err := s.ListCategories(ctx, userID)
	if err != nil {
		t.Fatalf("ListCategories() = %v", err)
	}

	want := []store.Category{
		{Name: "Comics", Feeds: 2, Unread: 1},
		{Name: "Tech", Feeds: 1, Unread: 1},
		// The nameless one sorts last, whatever it is called on screen.
		{Name: "", Feeds: 1, Unread: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("ListCategories() returned %d categories, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("category %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// One reader's filing is not visible through another's category filter.
//
// The case that matters is a *shared* article — one both readers subscribe to,
// so it is legitimately visible to both — filed under different folder names by
// each of them. Both halves of that are needed to make the test mean anything:
//
//   - An article only Bob can see proves nothing, because visibleArticles already
//     excludes it. A test built that way passes with the category filter's user
//     scoping deleted, which is how it looks like coverage while being none.
//   - With a shared article, deleting `f4.user_id = $1` makes Bob's category names
//     into working filters over Alice's own articles: she would find her article
//     under a folder she never created, which is a readable trace of somebody
//     else's taxonomy.
func TestOneReadersCategoriesAreInvisibleToAnother(t *testing.T) {
	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	// A second user, created directly: there is no signup flow, and the isolation
	// tests do the same.
	var bob store.UserID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("creating the second user: %v", err)
	}

	published := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	shared, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://example.com/shared-strip",
		Title:        "A strip they both subscribe to",
		PublishedAt:  &published,
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	insertBody(t, s, shared, store.ContentParams{Text: "body of the shared strip"})

	// The same feed URL, filed under a different folder by each reader. Feeds are
	// per-user rows, so this is two rows with two categories.
	for _, sub := range []struct {
		user     store.UserID
		category string
	}{
		{alice, "Comics"},
		{bob, "Bob's Secret Folder"},
	} {
		feedID, _, err := s.UpsertFeed(ctx, sub.user, store.FeedParams{
			FeedURL: "https://example.com/strip/feed.xml", Title: "The Strip", Category: sub.category,
		})
		if err != nil {
			t.Fatalf("UpsertFeed(%q) = %v", sub.category, err)
		}
		if _, err := s.InsertFeedItem(ctx, sub.user, store.FeedItemParams{
			FeedID: feedID, ArticleID: shared, GUID: "guid-shared",
		}); err != nil {
			t.Fatalf("InsertFeedItem(%q) = %v", sub.category, err)
		}
	}

	// Her own folder finds it: the article really is visible to her, so a later
	// empty result cannot be explained away by the article being out of reach.
	hers, err := s.Stream(ctx, alice, store.StreamQuery{Category: "Comics", Categorized: true})
	if err != nil {
		t.Fatalf("Stream(Comics) = %v", err)
	}
	if len(hers) != 1 || hers[0].ArticleID != shared {
		t.Fatalf("Comics for Alice = %+v, want the shared article %d", hers, shared)
	}

	// His folder does not, even though the article is hers to read.
	his, err := s.Stream(ctx, alice, store.StreamQuery{
		Category: "Bob's Secret Folder", Categorized: true,
	})
	if err != nil {
		t.Fatalf("Stream(Bob's folder) = %v", err)
	}
	if len(his) != 0 {
		t.Errorf("Alice found %d articles under another reader's category name, want 0", len(his))
	}
}

// The empty category selects the feeds with no category, and nothing else. A bare
// empty string cannot express this, which is why Categorized exists — and a
// regression here would quietly turn the filter off and show the whole archive.
func TestEmptyCategorySelectsOnlyUnfiledFeeds(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedCategorized(t, s, userID, "Comics", "xkcd", "xkcd", base)
	loose := seedCategorized(t, s, userID, "", "Loose", "loose", base.Add(time.Minute))

	items, err := s.Stream(ctx, userID, store.StreamQuery{Category: "", Categorized: true})
	if err != nil {
		t.Fatalf("Stream() = %v", err)
	}
	if len(items) != 1 || items[0].ArticleID != loose {
		t.Fatalf("the unfiled category = %+v, want only %d", items, loose)
	}

	// And with Categorized false the same empty name filters nothing.
	all, err := s.Stream(ctx, userID, store.StreamQuery{Category: ""})
	if err != nil {
		t.Fatalf("Stream() = %v", err)
	}
	if len(all) != 2 {
		t.Errorf("an unset category filter returned %d articles, want 2", len(all))
	}
}

func TestFeedsInCategory(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedCategorized(t, s, userID, "Comics", "xkcd", "xkcd", base)
	seedCategorized(t, s, userID, "Comics", "MonkeyUser", "monkeyuser", base.Add(time.Minute))
	seedCategorized(t, s, userID, "Tech", "Fowler", "fowler", base.Add(2*time.Minute))

	feeds, err := s.FeedsInCategory(ctx, userID, "Comics")
	if err != nil {
		t.Fatalf("FeedsInCategory() = %v", err)
	}
	if len(feeds) != 2 {
		t.Fatalf("FeedsInCategory(Comics) returned %d feeds, want 2: %+v", len(feeds), feeds)
	}
	if feeds[0].Title != "MonkeyUser" || feeds[1].Title != "xkcd" {
		t.Errorf("FeedsInCategory(Comics) = %q, %q; want them ordered by title",
			feeds[0].Title, feeds[1].Title)
	}
}
