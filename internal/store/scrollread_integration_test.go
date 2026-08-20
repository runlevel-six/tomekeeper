package store_test

import (
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// idsOf pulls the article ids out of what an automatic mark reported writing.
func idsOf(marked []store.MarkedRead) map[store.ArticleID]bool {
	out := make(map[store.ArticleID]bool, len(marked))
	for _, m := range marked {
		out[m.ArticleID] = true
	}
	return out
}

// The exclusion the feature was asked for: scrolling past something you starred or
// saved is not a decision to be finished with it.
//
// Enforced in the store rather than in the page's script, which is the point of the
// test. The script decides when a row went past; if it were also trusted to decide
// which rows may be marked, then any request — a stale tab, a retry, something
// hand-made — would mark a starred article read with nothing to stop it.
func TestMarkReadAutomaticallySkipsStarredAndSaved(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	plain := seedCategorized(t, s, userID, "Comics", "xkcd", "xkcd", base)
	starred := seedCategorized(t, s, userID, "Comics", "MonkeyUser", "monkeyuser", base.Add(time.Minute))
	saved := seedCategorized(t, s, userID, "Comics", "Oatmeal", "oatmeal", base.Add(2*time.Minute))
	kept := seedCategorized(t, s, userID, "Comics", "CommitStrip", "commitstrip", base.Add(3*time.Minute))

	if _, err := s.SetStarred(ctx, userID, starred, true); err != nil {
		t.Fatalf("SetStarred() = %v", err)
	}
	// Starring stamps saved_at as well, so a starred article is excluded twice over
	// and neither clause can be tested through it: deleting the starred one leaves
	// this case passing on the saved one. Hence a row that is starred and *not*
	// saved, written directly because nothing in the interface produces that state
	// today — which is the point. The starred clause must not be load-bearing only
	// by way of another method's side effect, or a Fever client that stars without
	// saving would quietly start losing stars to a scroll.
	starredUnsaved := seedCategorized(t, s, userID, "Comics", "Nerf NOW", "nerfnow", base.Add(4*time.Minute))
	if _, err := pool.Exec(ctx, `
		INSERT INTO article_state (user_id, article_id, starred, saved_at)
		VALUES ($1, $2, true, NULL)
		ON CONFLICT (user_id, article_id) DO UPDATE SET starred = true, saved_at = NULL`,
		userID, starredUnsaved); err != nil {
		t.Fatalf("starring an article without saving it: %v", err)
	}
	// Saved is what the reading list is built on, and what an import marks. Written
	// directly because the only way to save an article through the store is by URL,
	// and what matters here is the column: this one is saved *without* being starred,
	// so the test cannot pass on the starred clause alone.
	if _, err := pool.Exec(ctx, `
		INSERT INTO article_state (user_id, article_id, saved_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id, article_id) DO UPDATE SET saved_at = now()`,
		userID, saved); err != nil {
		t.Fatalf("marking an article saved: %v", err)
	}
	// Kept is deliberately *not* excluded: it protects the stored body from
	// retention, which says nothing about whether the reader has finished reading.
	if _, err := s.SetKept(ctx, userID, kept, true); err != nil {
		t.Fatalf("SetKept() = %v", err)
	}

	marked, err := s.MarkReadAutomatically(ctx, userID,
		[]store.ArticleID{plain, starred, starredUnsaved, saved, kept})
	if err != nil {
		t.Fatalf("MarkReadAutomatically() = %v", err)
	}

	got := idsOf(marked)
	if !got[plain] {
		t.Error("an ordinary unread article was not marked read")
	}
	if got[starred] {
		t.Error("a starred article was marked read by scrolling past it")
	}
	if got[starredUnsaved] {
		t.Error("a starred-but-unsaved article was marked read; the starred clause is not doing anything")
	}
	if got[saved] {
		t.Error("a saved article was marked read by scrolling past it")
	}
	if !got[kept] {
		t.Error("a kept article was skipped; keeping is about retention, not attention")
	}

	// And the database agrees with the report, which is what the interface redraws
	// its controls from.
	for _, tc := range []struct {
		name string
		id   store.ArticleID
		read bool
	}{
		{"ordinary", plain, true},
		{"starred", starred, false},
		{"starred without being saved", starredUnsaved, false},
		{"saved", saved, false},
		{"kept", kept, true},
	} {
		view, err := s.ArticleForUser(ctx, userID, tc.id)
		if err != nil {
			t.Fatalf("ArticleForUser(%s) = %v", tc.name, err)
		}
		if view.Read != tc.read {
			t.Errorf("%s article: read = %v, want %v", tc.name, view.Read, tc.read)
		}
	}
}

// Every marked row comes back with the rest of its state, because the caller
// redraws that row's controls from it.
//
// The lesson this encodes was learned the hard way once already: a shared partial
// rendered without one of its fields showed a kept article as not kept, correct in
// the database and wrong on screen until a reload. An automatic mark redraws the
// same partial, so Kept has to survive the round trip.
func TestMarkReadAutomaticallyReportsKept(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	plain := seedCategorized(t, s, userID, "Comics", "xkcd", "xkcd", base)
	kept := seedCategorized(t, s, userID, "Comics", "CommitStrip", "commitstrip", base.Add(time.Minute))

	if _, err := s.SetKept(ctx, userID, kept, true); err != nil {
		t.Fatalf("SetKept() = %v", err)
	}

	marked, err := s.MarkReadAutomatically(ctx, userID, []store.ArticleID{plain, kept})
	if err != nil {
		t.Fatalf("MarkReadAutomatically() = %v", err)
	}
	if len(marked) != 2 {
		t.Fatalf("MarkReadAutomatically() marked %d articles, want 2", len(marked))
	}

	for _, m := range marked {
		want := m.ArticleID == kept
		if m.Kept != want {
			t.Errorf("article %d reported Kept = %v, want %v", m.ArticleID, m.Kept, want)
		}
	}
}

// An article already read keeps the time it was first read.
//
// read_at is what retention measures from, so re-stamping it every time a finished
// article scrolls by would quietly keep the whole archive alive — and this is the
// path most likely to see an already-read article, since the unread list keeps them
// visible for half an hour after they are opened.
func TestMarkReadAutomaticallyKeepsTheFirstReadTime(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	article := seedCategorized(t, s, userID, "Comics", "xkcd", "xkcd", base)

	if _, err := s.SetRead(ctx, userID, article, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}

	readAt := func() time.Time {
		t.Helper()
		var at *time.Time
		if err := pool.QueryRow(ctx,
			`SELECT read_at FROM article_state WHERE user_id = $1 AND article_id = $2`,
			userID, article).Scan(&at); err != nil {
			t.Fatalf("reading read_at: %v", err)
		}
		if at == nil {
			t.Fatal("read_at is null for an article that was marked read")
		}
		return *at
	}
	was := readAt()

	marked, err := s.MarkReadAutomatically(ctx, userID, []store.ArticleID{article})
	if err != nil {
		t.Fatalf("MarkReadAutomatically() = %v", err)
	}
	// Nothing was written, so nothing needs redrawing — which is also what keeps a
	// reader scrolling back and forth over a read row from generating traffic.
	if len(marked) != 0 {
		t.Errorf("MarkReadAutomatically() reported %d marks for an already-read article, want 0", len(marked))
	}
	if now := readAt(); !now.Equal(was) {
		t.Errorf("read_at moved from %s to %s; scrolling past a read article must not re-stamp it", was, now)
	}
}

// One reader cannot mark another reader's articles read by naming their ids.
//
// The ids arrive from a page rather than from a query, so visibleArticles is the
// only thing bounding this to the reader's own archive. Built on an article Alice
// genuinely cannot see: if the predicate were removed, she would write a state row
// against Bob's article and learn it exists — one id at a time, which is exactly how
// somebody enumerates an archive they cannot read.
func TestMarkReadAutomaticallyCannotReachAnInvisibleArticle(t *testing.T) {
	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	var bob store.UserID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("creating the second user: %v", err)
	}

	// Bob's alone: his feed carries it, and Alice has no state row against it.
	hidden := seedCategorized(t, s, bob, "Bob's folder", "Bob's feed", "bobs-feed",
		time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))

	marked, err := s.MarkReadAutomatically(ctx, alice, []store.ArticleID{hidden})
	if err != nil {
		t.Fatalf("MarkReadAutomatically() = %v", err)
	}
	if len(marked) != 0 {
		t.Errorf("Alice marked %d of Bob's articles read, want 0", len(marked))
	}

	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM article_state WHERE article_id = $1`, hidden).Scan(&rows); err != nil {
		t.Fatalf("counting state rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d state rows exist against Bob's article, want 0", rows)
	}
}

// A story two of the reader's feeds carry is one article and one write.
//
// The same trap the bulk mark has: an id list plus a join over feed_items would
// present the same row twice in one statement, and Postgres refuses that outright.
// `= ANY(...)` is what makes this safe, and a duplicated id in the request is
// harmless for the same reason.
func TestMarkReadAutomaticallyHandlesAnArticleOnTwoFeeds(t *testing.T) {
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

	marked, err := s.MarkReadAutomatically(ctx, userID, []store.ArticleID{shared, shared})
	if err != nil {
		t.Fatalf("MarkReadAutomatically() = %v", err)
	}
	if len(marked) != 1 {
		t.Errorf("MarkReadAutomatically() reported %d marks, want 1", len(marked))
	}
}
