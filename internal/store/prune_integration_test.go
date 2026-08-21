package store_test

import (
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// prunableSet is the state prune exists for: an article whose feed is gone and which
// nobody ever touched.
// subscribed seeds a feed with one article on it, still subscribed.
//
// Split from the unsubscribe because the *order* matters and is easy to get wrong:
// state writes are guarded by the same visibility predicate reads are, so an article
// with no feed reference and no state row cannot acquire one. Acting on an article
// after unsubscribing is refused — silently, by design, so that a reader cannot
// confirm what exists one insert at a time.
//
// Which is a property worth knowing about prune: once "never acted on" holds for an
// orphan, nothing can make it false again.
func subscribed(t *testing.T, s *store.Store, userID store.UserID, slug string) (store.FeedID, store.ArticleID) {
	t.Helper()
	ctx := t.Context()

	feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://example.com/" + slug + "/feed.xml", Title: slug,
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	id := seedArticleWithBody(t, s, slug)
	if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
		FeedID: feedID, ArticleID: id, GUID: "guid-" + slug,
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
	return feedID, id
}

// unsubscribe is what leaves the residue: the subscription goes, its feed_items
// cascade with it, and the article stays.
func unsubscribe(t *testing.T, s *store.Store, userID store.UserID, feedID store.FeedID) {
	t.Helper()
	if removed, err := s.DeleteFeed(t.Context(), userID, feedID); err != nil {
		t.Fatalf("DeleteFeed() = %v", err)
	} else if !removed {
		t.Fatal("DeleteFeed removed nothing")
	}
}

func orphan(t *testing.T, s *store.Store, userID store.UserID, slug string) store.ArticleID {
	t.Helper()
	feedID, id := subscribed(t, s, userID, slug)
	unsubscribe(t, s, userID, feedID)
	return id
}

func seedArticleWithBody(t *testing.T, s *store.Store, slug string) store.ArticleID {
	t.Helper()
	ctx := t.Context()

	published := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	id, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://example.com/" + slug,
		Title:        slug,
		PublishedAt:  &published,
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if _, err := s.InsertContent(ctx, store.ContentParams{
		ArticleID: id, ExtractorName: "trafilatura", ExtractorVersion: "7",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>A body worth some bytes for " + slug + ".</p>",
		Text:          "A body worth some bytes for " + slug + ".",
		WordCount:     8,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}
	return id
}

func prunableIDs(t *testing.T, s *store.Store) map[store.ArticleID]int64 {
	t.Helper()
	got, err := s.PrunableArticles(t.Context(), 100)
	if err != nil {
		t.Fatalf("PrunableArticles() = %v", err)
	}
	out := make(map[store.ArticleID]int64, len(got))
	for _, p := range got {
		out[p.ArticleID] = p.Bytes
	}
	return out
}

// The residue, and only the residue. Retention cannot reach these: it requires an
// article to have been *read*, so one that arrived, was never opened, and then lost
// its feed is never expirable at any setting.
func TestPruneFindsWhatUnsubscribingLeftBehind(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)

	abandoned := orphan(t, s, alice, "abandoned")

	got := prunableIDs(t, s)
	if _, ok := got[abandoned]; !ok {
		t.Errorf("the article left behind by unsubscribing is not prunable: %v", got)
	}
	if got[abandoned] <= 0 {
		t.Errorf("it reports %d bytes; the number is what makes this a decision somebody can take", got[abandoned])
	}
}

// Each exclusion is its own case, because each is a separate promise and a shared
// condition would hide a missing one.
func TestPruneRefusesWhatSomebodyStillHas(t *testing.T) {
	t.Run("an article a feed still references", func(t *testing.T) {
		_, s, alice := dbtest.SetupWithUser(t)
		ctx := t.Context()

		feedID, _, err := s.UpsertFeed(ctx, alice, store.FeedParams{
			FeedURL: "https://example.com/live/feed.xml", Title: "live",
		})
		if err != nil {
			t.Fatalf("UpsertFeed() = %v", err)
		}
		id := seedArticleWithBody(t, s, "live")
		if _, err := s.InsertFeedItem(ctx, alice, store.FeedItemParams{
			FeedID: feedID, ArticleID: id, GUID: "guid-live",
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}

		if _, ok := prunableIDs(t, s)[id]; ok {
			t.Error("a subscribed article is prunable")
		}
	})

	for _, tc := range []struct {
		name string
		act  func(t *testing.T, s *store.Store, u store.UserID, id store.ArticleID)
	}{
		{"read", func(t *testing.T, s *store.Store, u store.UserID, id store.ArticleID) {
			if _, err := s.SetRead(t.Context(), u, id, true); err != nil {
				t.Fatalf("SetRead() = %v", err)
			}
		}},
		{"starred", func(t *testing.T, s *store.Store, u store.UserID, id store.ArticleID) {
			if _, err := s.SetStarred(t.Context(), u, id, true); err != nil {
				t.Fatalf("SetStarred() = %v", err)
			}
		}},
	} {
		t.Run("an article somebody "+tc.name, func(t *testing.T) {
			_, s, alice := dbtest.SetupWithUser(t)

			// Acted on *while still subscribed*, which is the real order of events
			// and the only order that works: a state write on an article with no feed
			// reference and no state row is refused by the visibility predicate. The
			// first version of this test acted afterwards, so nothing was written and
			// the article was prunable — which the test then reported as a bug in
			// prune rather than in itself.
			feedID, id := subscribed(t, s, alice, "touched-"+tc.name)
			tc.act(t, s, alice, id)
			unsubscribe(t, s, alice, feedID)

			if _, ok := prunableIDs(t, s)[id]; ok {
				t.Errorf("an article somebody %s is prunable, and it is still reachable through their lists", tc.name)
			}
		})
	}

	t.Run("an imported body", func(t *testing.T) {
		_, s, alice := dbtest.SetupWithUser(t)
		ctx := t.Context()

		id := orphan(t, s, alice, "imported")
		_ = ctx
		// An imported body may be the only surviving copy of a page that is gone, so
		// releasing it is not "it can be fetched again" — it is losing the article.
		// Retention states the same rule for the same reason.
		if _, err := s.InsertContent(ctx, store.ContentParams{
			ArticleID: id, ExtractorName: "imported", ExtractorVersion: "wallabag",
			ContentOrigin: store.OriginImport("wallabag"), Immutable: true,
			HTML: "<p>The only copy there is.</p>", Text: "The only copy there is.",
			WordCount: 5,
		}); err != nil {
			t.Fatalf("InsertContent() = %v", err)
		}

		if _, ok := prunableIDs(t, s)[id]; ok {
			t.Error("an article with an imported body is prunable")
		}
	})
}
