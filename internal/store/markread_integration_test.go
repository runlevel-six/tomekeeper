package store_test

import (
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// unreadCategory is the query the category stream is drawn with, which is also the
// query a bulk mark from that page is scoped by. Written once here so a test cannot
// assert against a filter the interface does not use.
func unreadCategory(name string) store.StreamQuery {
	return store.StreamQuery{Category: name, Categorized: true}
}

// Marking one list read marks that list, and stops there.
func TestMarkReadInMarksOnlyTheListItWasGiven(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedCategorized(t, s, userID, "Comics", "xkcd", "xkcd", base)
	seedCategorized(t, s, userID, "Comics", "MonkeyUser", "monkeyuser", base.Add(time.Minute))
	tech := seedCategorized(t, s, userID, "Tech", "Fowler", "fowler", base.Add(2*time.Minute))

	// The count the reader is shown before they commit, and the number of articles
	// the commit then affects. Asserted together because a control that promises 2
	// and marks 3 is worse than no control.
	before, err := s.CountUnreadIn(ctx, userID, unreadCategory("Comics"))
	if err != nil {
		t.Fatalf("CountUnreadIn() = %v", err)
	}
	if before != 2 {
		t.Fatalf("CountUnreadIn(Comics) = %d, want 2", before)
	}

	marked, err := s.MarkReadIn(ctx, userID, unreadCategory("Comics"))
	if err != nil {
		t.Fatalf("MarkReadIn() = %v", err)
	}
	if marked != before {
		t.Errorf("MarkReadIn(Comics) marked %d articles, but the reader was offered %d", marked, before)
	}

	// Comics is empty of unread, Tech is untouched.
	if n, err := s.CountUnreadIn(ctx, userID, unreadCategory("Comics")); err != nil || n != 0 {
		t.Errorf("Comics still has %d unread (err %v), want 0", n, err)
	}
	if n, err := s.CountUnreadIn(ctx, userID, unreadCategory("Tech")); err != nil || n != 1 {
		t.Errorf("Tech has %d unread (err %v), want the 1 it started with", n, err)
	}

	view, err := s.ArticleForUser(ctx, userID, tech)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if view.Read {
		t.Error("marking Comics read also marked the article in Tech")
	}

	// Re-posting the same mark is a no-op that reports itself honestly, which is
	// what makes rendering the result rather than redirecting safe.
	again, err := s.MarkReadIn(ctx, userID, unreadCategory("Comics"))
	if err != nil {
		t.Fatalf("MarkReadIn() twice = %v", err)
	}
	if again != 0 {
		t.Errorf("marking Comics read a second time marked %d articles, want 0", again)
	}
}

// A bulk mark leaves an already-read article's read timestamp alone.
//
// This is not tidiness. read_at is what the retention policy measures from, so
// pushing it forward on every bulk mark would quietly extend the life of
// everything the reader had already finished — and, the other way round, stamping
// a whole list with one timestamp means anything genuinely read earlier keeps its
// own place in the queue rather than being dragged along with the batch.
func TestMarkReadInDoesNotDisturbAnEarlierRead(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	early := seedCategorized(t, s, userID, "Comics", "xkcd", "xkcd", base)
	seedCategorized(t, s, userID, "Comics", "MonkeyUser", "monkeyuser", base.Add(time.Minute))

	if _, err := s.SetRead(ctx, userID, early, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}

	readAt := func() time.Time {
		t.Helper()
		var at *time.Time
		if err := pool.QueryRow(ctx,
			`SELECT read_at FROM article_state WHERE user_id = $1 AND article_id = $2`,
			userID, early).Scan(&at); err != nil {
			t.Fatalf("reading read_at: %v", err)
		}
		if at == nil {
			t.Fatal("read_at is null for an article that was marked read")
		}
		return *at
	}
	was := readAt()

	marked, err := s.MarkReadIn(ctx, userID, unreadCategory("Comics"))
	if err != nil {
		t.Fatalf("MarkReadIn() = %v", err)
	}
	if marked != 1 {
		t.Errorf("MarkReadIn(Comics) marked %d articles, want only the 1 that was unread", marked)
	}
	if now := readAt(); !now.Equal(was) {
		t.Errorf("read_at moved from %s to %s; a bulk mark must not re-stamp an earlier read", was, now)
	}
}

// A syndicated article carried by two of the reader's feeds is one article, and one
// row to write.
//
// The trap this guards is specific: if the mark selected articles by joining
// feed_items rather than by an EXISTS, this article would arrive twice in one
// statement and Postgres would refuse it outright — "ON CONFLICT DO UPDATE command
// cannot affect row a second time". Two feeds carrying the same story is the
// ordinary case in a real subscription list, so the failure would be immediate and
// total for the readers most likely to want this control.
func TestMarkReadInHandlesAnArticleOnTwoFeeds(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	published := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	shared, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://example.com/syndicated",
		Title:        "A story two feeds carried",
		PublishedAt:  &published,
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	insertBody(t, s, shared, store.ContentParams{Text: "body of the syndicated story"})

	for _, feed := range []string{"first", "second"} {
		feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
			FeedURL: "https://example.com/" + feed + "/feed.xml", Title: feed, Category: "News",
		})
		if err != nil {
			t.Fatalf("UpsertFeed(%s) = %v", feed, err)
		}
		if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
			FeedID: feedID, ArticleID: shared, GUID: "guid-" + feed,
		}); err != nil {
			t.Fatalf("InsertFeedItem(%s) = %v", feed, err)
		}
	}

	// Counted as one article by the control, and written as one row by the mark.
	if n, err := s.CountUnreadIn(ctx, userID, unreadCategory("News")); err != nil || n != 1 {
		t.Fatalf("CountUnreadIn(News) = %d (err %v), want 1", n, err)
	}
	marked, err := s.MarkReadIn(ctx, userID, unreadCategory("News"))
	if err != nil {
		t.Fatalf("MarkReadIn() = %v", err)
	}
	if marked != 1 {
		t.Errorf("MarkReadIn(News) marked %d articles, want 1", marked)
	}
}

// One reader's bulk mark cannot reach through another reader's filing.
//
// Built on a *shared* article, which is the only shape that means anything here —
// the same reasoning as the category isolation test above. An article only Bob can
// see is already excluded by visibleArticles, so a test built that way would pass
// with the category filter's own user scoping deleted. With a shared article, two
// distinct clauses are actually load-bearing and both are checked:
//
//   - Deleting `f4.user_id = $1` from the category filter would let Alice mark her
//     own copy of the article read by naming a folder only Bob created.
//   - Writing the state row against anything but Alice's user id would mark it read
//     for Bob, who never asked and cannot tell why his article turned grey.
func TestMarkReadInIsScopedToOneReader(t *testing.T) {
	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

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

	// Bob's folder name is not a filter Alice can mark through, even though the
	// article behind it is legitimately hers to read.
	marked, err := s.MarkReadIn(ctx, alice, unreadCategory("Bob's Secret Folder"))
	if err != nil {
		t.Fatalf("MarkReadIn(Bob's folder) = %v", err)
	}
	if marked != 0 {
		t.Errorf("Alice marked %d articles read through another reader's category name, want 0", marked)
	}

	// Her own folder does mark it — so the zero above is a scoping result rather
	// than an article that was out of reach all along.
	if marked, err := s.MarkReadIn(ctx, alice, unreadCategory("Comics")); err != nil || marked != 1 {
		t.Fatalf("MarkReadIn(Comics) = %d (err %v), want 1", marked, err)
	}

	// And it stayed unread for Bob, who shares the article but not the decision.
	his, err := s.ArticleForUser(ctx, bob, shared)
	if err != nil {
		t.Fatalf("ArticleForUser(bob) = %v", err)
	}
	if his.Read {
		t.Error("Alice's bulk mark marked the shared article read for Bob as well")
	}
	if n, err := s.CountUnreadIn(ctx, bob, unreadCategory("Bob's Secret Folder")); err != nil || n != 1 {
		t.Errorf("Bob's folder has %d unread (err %v), want the 1 it started with", n, err)
	}
}

// The archive-wide lists are held in by the access boundary and nothing else.
//
// This is the case the category test above cannot make. A category filter carries
// its own user scoping, so it would keep working even if visibleArticles were
// removed entirely; the unread stream and Everything carry no filter at all beyond
// that predicate, and they are both markable. Marking them read is therefore the
// one path where a bulk write is bounded by visibleArticles alone — and a state row
// written against an article the reader cannot see is exactly what the scoping
// discipline forbids, at 10,000 rows a click rather than one insert at a time.
//
// Neutering visibleArticles to `(true)` must fail this test. When it did not, this
// test did not exist.
func TestMarkReadInCannotReachAnInvisibleArticle(t *testing.T) {
	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	var bob store.UserID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("creating the second user: %v", err)
	}

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedCategorized(t, s, alice, "Comics", "xkcd", "hers", base)
	seedCategorized(t, s, bob, "Bob's Folder", "Bob's Feed", "his", base.Add(time.Minute))

	// Everything, unfiltered: the widest list the interface offers, and the widest
	// query a mark can be handed.
	before, err := s.CountUnreadIn(ctx, alice, store.StreamQuery{})
	if err != nil {
		t.Fatalf("CountUnreadIn() = %v", err)
	}
	if before != 1 {
		t.Fatalf("Alice's whole archive counts %d unread, want only her own 1", before)
	}

	marked, err := s.MarkReadIn(ctx, alice, store.StreamQuery{})
	if err != nil {
		t.Fatalf("MarkReadIn() = %v", err)
	}
	if marked != 1 {
		t.Errorf("marking Alice's whole archive read wrote %d rows, want only her own 1", marked)
	}

	// Nothing was written against Bob's article — not a read row, not a row at all.
	var rows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM article_state st
		  JOIN articles a ON a.id = st.article_id
		WHERE st.user_id = $1 AND a.url_canonical = 'https://example.com/his'`,
		alice).Scan(&rows); err != nil {
		t.Fatalf("counting Alice's state rows against Bob's article: %v", err)
	}
	if rows != 0 {
		t.Errorf("Alice has %d state rows against an article she cannot see, want 0", rows)
	}

	if n, err := s.CountUnreadIn(ctx, bob, store.StreamQuery{}); err != nil || n != 1 {
		t.Errorf("Bob has %d unread (err %v), want the 1 he started with", n, err)
	}
}

// The bulk mark acts on the whole list, not on the page the reader can see.
//
// A stream page is 50 rows and carries a cursor; a mark that inherited either would
// leave a list the reader was told they had emptied still holding articles, which
// is the sort of thing that gets noticed as "it does not work" rather than as a
// paging bug.
func TestMarkReadInIgnoresPaging(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	const count = store.DefaultStreamLimit + 10
	for i := range count {
		seedCategorized(t, s, userID, "Comics", "feed", "strip-"+string(rune('a'+i%26))+string(rune('a'+i/26)),
			base.Add(time.Duration(i)*time.Minute))
	}

	// A query carrying exactly what a stream page would carry: a page size, and a
	// cursor positioned partway down the list.
	q := unreadCategory("Comics")
	q.Limit = store.DefaultStreamLimit
	q.BeforeSort, q.BeforeID = base.Add(5*time.Minute), 0

	if n, err := s.CountUnreadIn(ctx, userID, q); err != nil || n != count {
		t.Errorf("CountUnreadIn() with paging = %d (err %v), want the whole list of %d", n, err, count)
	}
	marked, err := s.MarkReadIn(ctx, userID, q)
	if err != nil {
		t.Fatalf("MarkReadIn() = %v", err)
	}
	if marked != count {
		t.Errorf("MarkReadIn() with paging marked %d articles, want the whole list of %d", marked, count)
	}
}
