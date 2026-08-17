package store_test

import (
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// seedStream creates n articles on one feed, published a minute apart so their
// order is unambiguous, and returns them newest-first.
func seedStream(t *testing.T, s *store.Store, userID store.UserID, n int) []store.ArticleID {
	t.Helper()
	ctx := t.Context()

	feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://example.com/feed.xml", Title: "Example",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	newest := make([]store.ArticleID, 0, n)

	for i := range n {
		published := base.Add(time.Duration(i) * time.Minute)

		id, _, err := s.UpsertArticle(ctx, store.ArticleParams{
			URLCanonical: "https://example.com/post-" + time.Duration(i).String(),
			Title:        "Post " + published.Format("15:04"),
			PublishedAt:  &published,
		})
		if err != nil {
			t.Fatalf("UpsertArticle() = %v", err)
		}
		insertBody(t, s, id, store.ContentParams{Text: "body of post"})
		if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
			FeedID: feedID, ArticleID: id, GUID: "guid-" + published.Format("1504"),
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}
		// Prepend: the newest is published last.
		newest = append([]store.ArticleID{id}, newest...)
	}
	return newest
}

// Paging must visit every article exactly once. An off-by-one in the cursor would
// silently skip articles, which in a reader means never seeing something you were
// subscribed to — and nothing would report it.
func TestStreamPagingVisitsEveryArticleOnce(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	const total = 25
	want := seedStream(t, s, userID, total)

	var got []store.ArticleID
	q := store.StreamQuery{Limit: 7}

	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("paging did not terminate")
		}
		items, err := s.Stream(ctx, userID, q)
		if err != nil {
			t.Fatalf("Stream() = %v", err)
		}
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			got = append(got, it.ArticleID)
		}
		last := items[len(items)-1]
		q.BeforeSort, q.BeforeID = last.SortAt, last.ArticleID
	}

	if len(got) != total {
		t.Fatalf("paging returned %d articles, want %d", len(got), total)
	}

	seen := make(map[store.ArticleID]int, total)
	for _, id := range got {
		seen[id]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("article %d appeared %d times across pages", id, n)
		}
	}

	// And in the documented order: newest first.
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d is article %d, want %d — the stream is not newest-first", i, got[i], want[i])
			break
		}
	}
}

func TestStreamFilters(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	ids := seedStream(t, s, userID, 5)
	read, starred := ids[0], ids[1]

	if _, err := s.SetRead(ctx, userID, read, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}
	if _, err := s.SetStarred(ctx, userID, starred, true); err != nil {
		t.Fatalf("SetStarred() = %v", err)
	}

	count := func(q store.StreamQuery) []store.StreamItem {
		t.Helper()
		items, err := s.Stream(ctx, userID, q)
		if err != nil {
			t.Fatalf("Stream() = %v", err)
		}
		return items
	}

	if got := count(store.StreamQuery{}); len(got) != 5 {
		t.Errorf("unfiltered stream has %d articles, want 5", len(got))
	}

	unread := count(store.StreamQuery{UnreadOnly: true})
	if len(unread) != 4 {
		t.Errorf("unread stream has %d articles, want 4", len(unread))
	}
	for _, it := range unread {
		if it.ArticleID == read {
			t.Error("the read article is still in the unread stream")
		}
	}

	star := count(store.StreamQuery{StarredOnly: true})
	if len(star) != 1 || star[0].ArticleID != starred {
		t.Errorf("starred stream = %+v, want just the starred article", star)
	}

	// A tag filter restricts to that tag.
	tagID, err := s.EnsureTag(ctx, userID, "keep")
	if err != nil {
		t.Fatalf("EnsureTag() = %v", err)
	}
	if ok, err := s.TagArticle(ctx, userID, ids[2], tagID); err != nil || !ok {
		t.Fatalf("TagArticle() = %v, %v", ok, err)
	}
	tagged := count(store.StreamQuery{TagID: tagID})
	if len(tagged) != 1 || tagged[0].ArticleID != ids[2] {
		t.Errorf("tag-filtered stream = %+v, want just the tagged article", tagged)
	}
}

// The excerpt keeps the stream cheap; a full body per row at 10,000 articles is a
// download rather than a page.
func TestStreamExcerptIsTruncated(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://example.com/f.xml", Title: "F",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	long := ""
	for range 200 {
		long += "long body text. "
	}

	id, _, err := s.UpsertArticle(ctx, store.ArticleParams{URLCanonical: "https://example.com/long"})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	insertBody(t, s, id, store.ContentParams{Text: long, WordCount: 600})
	if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
		FeedID: feedID, ArticleID: id, GUID: "g",
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}

	items, err := s.Stream(ctx, userID, store.StreamQuery{})
	if err != nil {
		t.Fatalf("Stream() = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if got := len(items[0].Excerpt); got != store.ExcerptLength {
		t.Errorf("excerpt is %d characters, want it truncated to %d", got, store.ExcerptLength)
	}
	if items[0].WordCount == 0 {
		t.Error("word count is zero for an article with a body")
	}
	if !items[0].HasBody {
		t.Error("HasBody is false for an article with a body")
	}
}

// A caller asking for the whole archive in one page gets the cap, not the archive.
func TestStreamLimitIsCapped(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)

	seedStream(t, s, userID, 3)

	items, err := s.Stream(t.Context(), userID, store.StreamQuery{Limit: 100000})
	if err != nil {
		t.Fatalf("Stream() = %v", err)
	}
	if len(items) != 3 {
		t.Errorf("got %d items, want the 3 that exist", len(items))
	}
}
