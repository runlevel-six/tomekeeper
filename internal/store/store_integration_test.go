package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// These tests require a live PostgreSQL instance. They skip when
// TOME_TEST_DATABASE_URL is unset; CI always sets it.

func TestMigrationsAreIdempotent(t *testing.T) {
	// Setup migrates. Doing it again must be a no-op rather than an error,
	// because the migration Job runs on every deployment.
	pool, _, _ := dbtest.SetupWithUser(t)

	var tables int
	err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name IN
			('users','feeds','articles','article_content','feed_items','article_state',
			 'assets','article_assets','domain_rules','tags','article_tags','highlights',
			 'import_records')`).Scan(&tables)
	if err != nil {
		t.Fatalf("counting tables: %v", err)
	}
	if want := 13; tables != want {
		t.Errorf("found %d of the expected tables, want %d", tables, want)
	}
}

// Seeding is repeatable, and it must not rename an account somebody has renamed.
//
// This test asserted the opposite until 2026-08-23, because configuration used to be
// the only thing that could name an account. Then a reader renamed themselves from
// Settings, signed out, signed back in as the new name — and the next release put the
// old one back, because `tome migrate` runs on every deploy and wrote TOME_USERNAME
// over the row. The rename also rewrites the Fever API key, so what was left was an
// account whose stored key belonged to a username the web interface no longer knew.
//
// So the contract is now: create if absent, otherwise leave alone and report what the
// account is actually called.
func TestEnsureSeedUserDoesNotRenameAnExistingAccount(t *testing.T) {
	_, s, first := dbtest.SetupWithUser(t)

	second, name, err := s.System().EnsureSeedUser(t.Context(), "tome")
	if err != nil {
		t.Fatalf("EnsureSeedUser() = %v", err)
	}
	if first != second {
		t.Errorf("second call returned id %d, want %d", second, first)
	}
	if name != "tome" {
		t.Errorf("name = %q, want the account's own name", name)
	}

	// The reader renames themselves, which is what Settings does.
	// The Fever key travels with the name, which is the second half of what the reset
	// broke: a key computed for one username left on an account called another.
	if err := s.System().SetUsername(t.Context(), first, "jason", "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("SetUsername() = %v", err)
	}

	// A deploy: the migration Job runs again with the configured name unchanged.
	same, name, err := s.System().EnsureSeedUser(t.Context(), "tome")
	if err != nil {
		t.Fatalf("EnsureSeedUser() after a rename = %v", err)
	}
	if same != first {
		t.Errorf("seeding created user id %d, want the existing %d", same, first)
	}
	if name != "jason" {
		t.Errorf("the account is called %q after a deploy, want the name the reader chose; "+
			"configuration overwrote it, which is the bug this test exists for", name)
	}
	if _, err := s.System().LookupUser(t.Context(), "jason"); err != nil {
		t.Errorf("LookupUser(the chosen name) = %v", err)
	}
	if _, err := s.System().LookupUser(t.Context(), "tome"); err == nil {
		t.Error("the configured name still resolves, so the row was written after all")
	}
}

// Seeding with an explicit id=1 does not advance the bigserial sequence, so
// without the fix in EnsureSeedUser the first user a signup flow creates would
// collide.
func TestSeedUserAdvancesTheIDSequence(t *testing.T) {
	pool, _, _ := dbtest.SetupWithUser(t)

	var id int64
	err := pool.QueryRow(t.Context(),
		`INSERT INTO users (username) VALUES ('second') RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("inserting a second user: %v — the id sequence was not reset past the seed user", err)
	}
	if id <= 1 {
		t.Errorf("second user got id %d, want greater than the seed user's 1", id)
	}
}

// The acceptance criterion: duplicate articles across feeds collapse to one
// articles row.
func TestArticleDeduplicationAcrossFeeds(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	feedA, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://a.example.com/feed", Title: "A",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	feedB, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://b.example.com/feed", Title: "B",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	const canonical = "https://example.com/shared-story"

	first, created, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: canonical,
		URLOriginal:  canonical + "?utm_source=a",
		Title:        "Shared Story",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if !created {
		t.Error("the first upsert reported the article as pre-existing")
	}

	second, created, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: canonical,
		URLOriginal:  canonical + "?utm_source=b",
		Title:        "Shared Story (syndicated)",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if created {
		t.Error("the second upsert created a new article, want the existing one")
	}
	if first != second {
		t.Errorf("second upsert returned id %d, want %d", second, first)
	}

	// Both feeds reference the one article.
	for _, ref := range []struct {
		feedID store.FeedID
		guid   string
	}{{feedA, "a-1"}, {feedB, "b-1"}} {
		inserted, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
			FeedID: ref.feedID, ArticleID: first, GUID: ref.guid,
		})
		if err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}
		if !inserted {
			t.Errorf("feed %d did not record its reference", ref.feedID)
		}
	}

	count, err := s.CountArticles(ctx)
	if err != nil {
		t.Fatalf("CountArticles() = %v", err)
	}
	if count != 1 {
		t.Errorf("the archive holds %d articles, want 1", count)
	}
}

// Existing metadata wins; only gaps are filled. A feed that supplies an empty
// title must not be able to erase a good one.
func TestArticleUpsertFillsGapsWithoutClobbering(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	const canonical = "https://example.com/story"
	published := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	if _, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: canonical,
		Title:        "The Real Title",
	}); err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	if _, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: canonical,
		Title:        "", // a feed with no title
		Author:       "A. Writer",
		PublishedAt:  &published,
	}); err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	got, err := s.GetArticleByURL(ctx, canonical)
	if err != nil {
		t.Fatalf("GetArticleByURL() = %v", err)
	}
	if got.Title != "The Real Title" {
		t.Errorf("Title = %q, want the original %q", got.Title, "The Real Title")
	}
	if got.Author != "A. Writer" {
		t.Errorf("Author = %q, want the newly supplied value", got.Author)
	}
	if got.PublishedAt == nil || !got.PublishedAt.Equal(published) {
		t.Errorf("PublishedAt = %v, want %v", got.PublishedAt, published)
	}
	if got.FetchStatus != "pending" {
		t.Errorf("FetchStatus = %q, want %q — polling leaves articles for the fetcher", got.FetchStatus, "pending")
	}
}

// The scoping discipline, enforced structurally rather than by convention. This
// is the ingest portion; the reading views are covered by the isolation tests in
// isolation_integration_test.go.
func TestUserScopingIsolatesFeedsAndItems(t *testing.T) {
	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	var bob store.UserID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("creating a second user: %v", err)
	}

	aliceFeed, _, err := s.UpsertFeed(ctx, alice, store.FeedParams{
		FeedURL: "https://private.example.com/feed", Title: "Alice's feed",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	// Bob cannot read Alice's feed, and gets "not found" rather than a
	// permission error: the existence of the row is itself information.
	if _, err := s.GetFeed(ctx, bob, aliceFeed); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetFeed as another user = %v, want pgx.ErrNoRows", err)
	}

	feeds, err := s.ListFeeds(ctx, bob)
	if err != nil {
		t.Fatalf("ListFeeds() = %v", err)
	}
	if len(feeds) != 0 {
		t.Errorf("another user's feed list has %d entries, want 0", len(feeds))
	}

	// The guard is in the query, not the caller: passing a feed id that
	// belongs to someone else inserts nothing.
	articleID, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://example.com/a",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	inserted, err := s.InsertFeedItem(ctx, bob, store.FeedItemParams{
		FeedID: aliceFeed, ArticleID: articleID, GUID: "smuggled",
	})
	if err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
	if inserted {
		t.Error("an item was attached to another user's feed")
	}

	// And Alice's own write still works, so the guard is not simply blocking
	// everything.
	inserted, err = s.InsertFeedItem(ctx, alice, store.FeedItemParams{
		FeedID: aliceFeed, ArticleID: articleID, GUID: "legitimate",
	})
	if err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
	if !inserted {
		t.Error("the owner could not add an item to their own feed")
	}

	// Articles are a shared pool, but reachability is not: Bob has no
	// subscription carrying this article.
	bobCount, err := s.CountUserArticles(ctx, bob)
	if err != nil {
		t.Fatalf("CountUserArticles() = %v", err)
	}
	if bobCount != 0 {
		t.Errorf("another user can reach %d articles, want 0", bobCount)
	}

	aliceCount, err := s.CountUserArticles(ctx, alice)
	if err != nil {
		t.Fatalf("CountUserArticles() = %v", err)
	}
	if aliceCount != 1 {
		t.Errorf("the owner can reach %d articles, want 1", aliceCount)
	}
}

func TestFeedUpsertIsIdempotent(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	params := store.FeedParams{
		FeedURL: "https://example.com/feed", Title: "Original", Category: "Tech",
	}

	first, created, err := s.UpsertFeed(ctx, userID, params)
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if !created {
		t.Error("the first upsert reported the feed as pre-existing")
	}

	// Simulate a poll having happened, so that the second import can be
	// checked for disturbing polling state.
	if err := s.RecordPollSuccess(ctx, userID, first, `"etag"`, "", 4*time.Hour); err != nil {
		t.Fatalf("RecordPollSuccess() = %v", err)
	}

	params.Title = "Renamed"
	second, created, err := s.UpsertFeed(ctx, userID, params)
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if created {
		t.Error("re-importing created a duplicate subscription")
	}
	if first != second {
		t.Errorf("re-import returned feed id %d, want %d", second, first)
	}

	f, err := s.GetFeed(ctx, userID, first)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if f.Title != "Renamed" {
		t.Errorf("Title = %q, want the re-imported %q", f.Title, "Renamed")
	}
	if f.ETag != `"etag"` {
		t.Errorf("ETag = %q, want re-import to leave polling state alone", f.ETag)
	}
	if f.PollInterval != 4*time.Hour {
		t.Errorf("PollInterval = %v, want re-import to leave it at 4h", f.PollInterval)
	}
}

func TestPollStateTransitions(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://example.com/feed", Title: "Feed",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	t.Run("success clears the failure state", func(t *testing.T) {
		if _, err := s.RecordPollFailure(ctx, userID, feedID, "boom", time.Hour, 20); err != nil {
			t.Fatalf("RecordPollFailure() = %v", err)
		}
		if err := s.RecordPollSuccess(ctx, userID, feedID, `"e"`, "Mon, 03 Aug 2026 10:00:00 GMT", time.Hour); err != nil {
			t.Fatalf("RecordPollSuccess() = %v", err)
		}

		f, err := s.GetFeed(ctx, userID, feedID)
		if err != nil {
			t.Fatalf("GetFeed() = %v", err)
		}
		if f.ConsecutiveFailures != 0 {
			t.Errorf("ConsecutiveFailures = %d after a success, want 0", f.ConsecutiveFailures)
		}
		if f.LastError != "" {
			t.Errorf("LastError = %q after a success, want empty", f.LastError)
		}
		if f.LastSuccessAt == nil {
			t.Error("LastSuccessAt is nil after a success")
		}
		if f.NextPollAt.Before(time.Now()) {
			t.Errorf("NextPollAt = %v, want a future time", f.NextPollAt)
		}
	})

	t.Run("failures accumulate and disable at the threshold", func(t *testing.T) {
		const threshold = 3

		for i := 1; i <= threshold; i++ {
			disabled, err := s.RecordPollFailure(ctx, userID, feedID, "HTTP 500", time.Hour, threshold)
			if err != nil {
				t.Fatalf("RecordPollFailure() = %v", err)
			}
			if want := i >= threshold; disabled != want {
				t.Errorf("after failure %d, disabled = %v, want %v", i, disabled, want)
			}
		}

		f, err := s.GetFeed(ctx, userID, feedID)
		if err != nil {
			t.Fatalf("GetFeed() = %v", err)
		}
		if !f.Disabled {
			t.Error("the feed is not disabled after reaching the threshold")
		}
		// The reason must survive, or the feed health view has nothing to show.
		if f.LastError != "HTTP 500" {
			t.Errorf("LastError = %q, want the recorded cause", f.LastError)
		}
	})

	t.Run("re-enabling clears the failure budget", func(t *testing.T) {
		if err := s.SetFeedDisabled(ctx, userID, feedID, false); err != nil {
			t.Fatalf("SetFeedDisabled() = %v", err)
		}

		f, err := s.GetFeed(ctx, userID, feedID)
		if err != nil {
			t.Fatalf("GetFeed() = %v", err)
		}
		if f.Disabled {
			t.Error("the feed is still disabled")
		}
		if f.ConsecutiveFailures != 0 {
			t.Errorf("ConsecutiveFailures = %d, want a fresh budget of 0", f.ConsecutiveFailures)
		}
	})
}

func TestDueFeeds(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	dueNow, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://due.example.com/feed", Title: "Due",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	notDue, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://later.example.com/feed", Title: "Later",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if err := s.RecordPollSuccess(ctx, userID, notDue, "", "", 6*time.Hour); err != nil {
		t.Fatalf("RecordPollSuccess() = %v", err)
	}

	disabled, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://disabled.example.com/feed", Title: "Disabled",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if err := s.SetFeedDisabled(ctx, userID, disabled, true); err != nil {
		t.Fatalf("SetFeedDisabled() = %v", err)
	}

	due, err := s.System().DueFeeds(ctx, 100)
	if err != nil {
		t.Fatalf("DueFeeds() = %v", err)
	}

	if len(due) != 1 {
		t.Fatalf("DueFeeds() returned %d feeds, want 1: %+v", len(due), due)
	}
	if due[0].FeedID != dueNow {
		t.Errorf("DueFeeds() returned feed %d, want %d", due[0].FeedID, dueNow)
	}
	if due[0].UserID != userID {
		t.Errorf("DueFeeds() returned user %d, want %d — the owner must travel with the feed",
			due[0].UserID, userID)
	}
}

// The schema's CHECK constraints are part of the contract, not decoration.
func TestFetchStatusIsConstrained(t *testing.T) {
	pool, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	id, _, err := s.UpsertArticle(ctx, store.ArticleParams{URLCanonical: "https://example.com/a"})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE articles SET fetch_status = 'nonsense' WHERE id = $1`, id); err == nil {
		t.Error("an invalid fetch_status was accepted, want the CHECK constraint to reject it")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE articles SET assets_status = 'nonsense' WHERE id = $1`, id); err == nil {
		t.Error("an invalid assets_status was accepted, want the CHECK constraint to reject it")
	}
}
