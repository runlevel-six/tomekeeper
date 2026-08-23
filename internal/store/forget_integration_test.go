package store_test

import (
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// forgetFixture gives a reader an article they have read, optionally reachable
// through a feed of theirs.
func forgetFixture(t *testing.T, s *store.Store, reader store.UserID, url string, viaFeed bool) store.ArticleID {
	t.Helper()

	id, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{URLCanonical: url, URLOriginal: url})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if viaFeed {
		feedID, _, err := s.UpsertFeed(t.Context(), reader, store.FeedParams{
			FeedURL: url + "/feed.xml", Title: "Example",
		})
		if err != nil {
			t.Fatalf("UpsertFeed() = %v", err)
		}
		if _, err := s.InsertFeedItem(t.Context(), reader, store.FeedItemParams{
			FeedID: feedID, ArticleID: id, GUID: url,
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}
	}
	if _, err := s.SetRead(t.Context(), reader, id, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}
	// Aged, rather than waiting out a retention window.
	if _, err := s.Pool().Exec(t.Context(),
		`UPDATE article_state SET read_at = now() - interval '90 days'
		 WHERE user_id = $1 AND article_id = $2`, reader, id); err != nil {
		t.Fatalf("aging the read time: %v", err)
	}
	return id
}

// An article a feed still carries keeps its row as a tombstone; one reachable only
// through the reader's own state loses the row entirely.
//
// The distinction is the whole design. ExpirableArticles reads "no state row" as
// *never seen it*, so deleting the row for a feed-carried article would make a
// reader who is finished look like one who has never opened it — and the article
// would never be expirable again.
func TestForgettingTombstonesOrDeletes(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)

	viaFeed := forgetFixture(t, s, alice, "https://forget.example/from-a-feed", true)

	// An article reachable only through this reader's own state, arrived at the way
	// it really happens: read from a feed, then unsubscribed. The feed_items rows go
	// with the subscription and the state row is left as the only thing referring to
	// it — which is also why unsubscribing leaves articles nothing collects.
	//
	// Not by reading an article with no feed at all: state writes are themselves
	// guarded by the visibility predicate, so a reader cannot mark an article read
	// that nothing makes visible to them.
	orphan := forgetFixture(t, s, alice, "https://forget.example/orphaned", true)
	if _, err := s.Pool().Exec(t.Context(),
		`DELETE FROM feeds WHERE user_id = $1 AND feed_url = $2`,
		alice, "https://forget.example/orphaned/feed.xml"); err != nil {
		t.Fatalf("unsubscribing: %v", err)
	}

	got, err := s.ForgetReadArticles(t.Context(), alice, time.Now().Add(-24*time.Hour), 50)
	if err != nil {
		t.Fatalf("ForgetReadArticles() = %v", err)
	}
	if got.Tombstoned != 1 || got.Deleted != 1 {
		t.Fatalf("forgot %+v, want one of each", got)
	}

	// The feed-carried one keeps a row that says only "finished".
	var (
		forgotten *time.Time
		readAt    *time.Time
		read      bool
	)
	if err := s.Pool().QueryRow(t.Context(),
		`SELECT forgotten_at, read_at, read FROM article_state WHERE user_id = $1 AND article_id = $2`,
		alice, viaFeed).Scan(&forgotten, &readAt, &read); err != nil {
		t.Fatalf("the feed-carried article lost its row: %v", err)
	}
	if forgotten == nil {
		t.Error("the row was not marked forgotten")
	}
	if readAt != nil {
		t.Error("the tombstone still records when it was read")
	}
	if !read {
		t.Error("the tombstone is not marked read, so the article would resurface as unread")
	}

	// The orphaned one is gone entirely, leaving the article referenced by nobody —
	// which is what `tome prune` collects.
	var rows int
	if err := s.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM article_state WHERE user_id = $1 AND article_id = $2`,
		alice, orphan).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 0 {
		t.Error("an article reachable only through this reader's state kept its row")
	}
}

// Everything a reader has said they still want is never forgotten.
func TestForgettingSkipsWhatIsStillWanted(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	starred := forgetFixture(t, s, alice, "https://forget.example/starred", true)
	if _, err := s.SetStarred(ctx, alice, starred, true); err != nil {
		t.Fatalf("SetStarred() = %v", err)
	}
	saved := forgetFixture(t, s, alice, "https://forget.example/saved", true)
	if _, err := s.Pool().Exec(ctx,
		`UPDATE article_state SET saved_at = now() WHERE user_id = $1 AND article_id = $2`,
		alice, saved); err != nil {
		t.Fatalf("saving: %v", err)
	}
	highlighted := forgetFixture(t, s, alice, "https://forget.example/highlighted", true)
	if _, err := s.AddHighlight(ctx, alice, highlighted,
		store.ImportHighlight{Quote: "a passage worth keeping"}); err != nil {
		t.Fatalf("AddHighlight() = %v", err)
	}
	// Read too recently: the window has not passed for this one.
	recent := forgetFixture(t, s, alice, "https://forget.example/recent", true)
	if _, err := s.Pool().Exec(ctx,
		`UPDATE article_state SET read_at = now() WHERE user_id = $1 AND article_id = $2`,
		alice, recent); err != nil {
		t.Fatalf("resetting the read time: %v", err)
	}
	// Read at an unknown time, which resolves toward keeping like everything else.
	unknown := forgetFixture(t, s, alice, "https://forget.example/unknown", true)
	if _, err := s.Pool().Exec(ctx,
		`UPDATE article_state SET read_at = NULL WHERE user_id = $1 AND article_id = $2`,
		alice, unknown); err != nil {
		t.Fatalf("clearing the read time: %v", err)
	}
	// And one that should go, so an empty result cannot pass this test.
	ordinary := forgetFixture(t, s, alice, "https://forget.example/ordinary", true)

	got, err := s.ForgetReadArticles(ctx, alice, time.Now().Add(-24*time.Hour), 50)
	if err != nil {
		t.Fatalf("ForgetReadArticles() = %v", err)
	}
	if got.Tombstoned+got.Deleted != 1 {
		t.Fatalf("forgot %+v, want only the ordinary article", got)
	}

	for name, id := range map[string]store.ArticleID{
		"starred": starred, "saved": saved, "highlighted": highlighted,
		"read recently": recent, "read at an unknown time": unknown,
	} {
		var forgotten *time.Time
		if err := s.Pool().QueryRow(ctx,
			`SELECT forgotten_at FROM article_state WHERE user_id = $1 AND article_id = $2`,
			alice, id).Scan(&forgotten); err != nil {
			t.Fatalf("%s: reading the row: %v", name, err)
		}
		if forgotten != nil {
			t.Errorf("a %s article was forgotten", name)
		}
	}

	var forgotten *time.Time
	if err := s.Pool().QueryRow(ctx,
		`SELECT forgotten_at FROM article_state WHERE user_id = $1 AND article_id = $2`,
		alice, ordinary).Scan(&forgotten); err != nil {
		t.Fatalf("reading the ordinary row: %v", err)
	}
	if forgotten == nil {
		t.Error("the ordinary article was not forgotten; the exclusions above prove nothing")
	}
}

// The article survives one reader forgetting it and goes when the last one does.
//
// This is the criterion in one test: a reader's window is theirs, and the bytes are
// the household's.
func TestAnArticleOutlivesOneReaderForgettingIt(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	bob, err := s.System().CreateUser(ctx, "bob", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}

	const url = "https://forget.example/shared"
	articleID := forgetFixture(t, s, alice, url, true)
	// Bob subscribes to his own feed carrying the same article and reads it too.
	bobFeed, _, err := s.UpsertFeed(ctx, bob, store.FeedParams{
		FeedURL: url + "/bob.xml", Title: "Example",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if _, err := s.InsertFeedItem(ctx, bob, store.FeedItemParams{
		FeedID: bobFeed, ArticleID: articleID, GUID: url,
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
	if _, err := s.SetRead(ctx, bob, articleID, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}
	if _, err := s.Pool().Exec(ctx,
		`UPDATE article_state SET read_at = now() - interval '90 days'
		 WHERE user_id = $1 AND article_id = $2`, bob, articleID); err != nil {
		t.Fatalf("aging bob's read time: %v", err)
	}
	// A body, because an article with none is not expirable at all.
	if _, err := s.InsertContent(ctx, store.ContentParams{
		ArticleID: articleID, Owner: store.Household(),
		ExtractorName: "trafilatura", ExtractorVersion: "6",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>shared</p>", Text: "shared", WordCount: 1,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}

	cutoff := time.Now().Add(-24 * time.Hour)

	// Alice forgets it. Bob has not — and that is now the whole test: a row that is
	// not forgotten is a claim however old it is, because Bob's window is Bob's.
	if _, err := s.ForgetReadArticles(ctx, alice, cutoff, 50); err != nil {
		t.Fatalf("ForgetReadArticles(alice) = %v", err)
	}
	expirable, err := s.ExpirableArticles(ctx, 50)
	if err != nil {
		t.Fatalf("ExpirableArticles() = %v", err)
	}
	for _, e := range expirable {
		if e.ArticleID == articleID {
			t.Fatal("the article became expirable while one reader still held it")
		}
	}

	// Bob forgets it too, and now nobody is holding on.
	if _, err := s.ForgetReadArticles(ctx, bob, cutoff, 50); err != nil {
		t.Fatalf("ForgetReadArticles(bob) = %v", err)
	}
	expirable, err = s.ExpirableArticles(ctx, 50)
	if err != nil {
		t.Fatalf("ExpirableArticles() = %v", err)
	}
	var found bool
	for _, e := range expirable {
		if e.ArticleID == articleID {
			found = true
		}
	}
	if !found {
		t.Error("the article is still not expirable after every reader forgot it")
	}
}

// A subscriber who has never opened it still blocks expiry, even once somebody else
// has forgotten it.
//
// The case the tombstone distinction exists for, and the one where getting it wrong
// is silent: an unread article deleted out from under the person who had not got to
// it yet.
func TestAnUnreadSubscriberStillBlocksExpiry(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	bob, err := s.System().CreateUser(ctx, "bob", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}

	const url = "https://forget.example/unread-elsewhere"
	articleID := forgetFixture(t, s, alice, url, true)

	// Bob subscribes and never opens it: no state row at all.
	bobFeed, _, err := s.UpsertFeed(ctx, bob, store.FeedParams{
		FeedURL: url + "/bob.xml", Title: "Example",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if _, err := s.InsertFeedItem(ctx, bob, store.FeedItemParams{
		FeedID: bobFeed, ArticleID: articleID, GUID: url,
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
	if _, err := s.InsertContent(ctx, store.ContentParams{
		ArticleID: articleID, Owner: store.Household(),
		ExtractorName: "trafilatura", ExtractorVersion: "6",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>unread</p>", Text: "unread", WordCount: 1,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}

	cutoff := time.Now().Add(-24 * time.Hour)
	if _, err := s.ForgetReadArticles(ctx, alice, cutoff, 50); err != nil {
		t.Fatalf("ForgetReadArticles() = %v", err)
	}

	expirable, err := s.ExpirableArticles(ctx, 50)
	if err != nil {
		t.Fatalf("ExpirableArticles() = %v", err)
	}
	for _, e := range expirable {
		if e.ArticleID == articleID {
			t.Error("an article somebody has not read yet became expirable because " +
				"another reader forgot it")
		}
	}
}

func TestReadersWithRetention(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	bob, err := s.System().CreateUser(ctx, "bob", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}

	// Nothing set anywhere: retention is off, and off means nobody is swept.
	readers, err := s.ReadersWithRetention(ctx, 0)
	if err != nil {
		t.Fatalf("ReadersWithRetention() = %v", err)
	}
	if len(readers) != 0 {
		t.Errorf("readers = %v with no retention configured, want none", readers)
	}

	// A household default reaches everybody who has not chosen otherwise.
	readers, err = s.ReadersWithRetention(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ReadersWithRetention() = %v", err)
	}
	if readers[alice] != 30*24*time.Hour || readers[bob] != 30*24*time.Hour {
		t.Errorf("readers = %v, want both on the household window", readers)
	}

	// A reader's own window wins, and zero is a real answer meaning keep
	// everything — distinct from having chosen nothing.
	own := 7 * 24 * time.Hour
	if err := s.System().SetRetention(ctx, alice, &own); err != nil {
		t.Fatalf("SetRetention() = %v", err)
	}
	none := time.Duration(0)
	if err := s.System().SetRetention(ctx, bob, &none); err != nil {
		t.Fatalf("SetRetention() = %v", err)
	}

	readers, err = s.ReadersWithRetention(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ReadersWithRetention() = %v", err)
	}
	if readers[alice] != own {
		t.Errorf("alice = %v, want her own %v", readers[alice], own)
	}
	if _, ok := readers[bob]; ok {
		t.Error("bob asked to keep everything and is being swept anyway")
	}
}
