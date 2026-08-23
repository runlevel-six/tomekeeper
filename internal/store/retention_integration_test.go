package store_test

import (
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// markRead sets read and read_at directly, because the cutoff is the whole
// subject of these tests and SetRead always stamps "now".
func markRead(t *testing.T, tr twoReaders, userID store.UserID, id store.ArticleID, when time.Time) {
	t.Helper()

	if _, err := tr.pool.Exec(t.Context(), `
		INSERT INTO article_state (user_id, article_id, read, read_at)
		VALUES ($1, $2, true, $3)
		ON CONFLICT (user_id, article_id) DO UPDATE
		SET read = true, read_at = EXCLUDED.read_at`, userID, id, when); err != nil {
		t.Fatalf("marking article %d read: %v", id, err)
	}
}

func expirableIDs(t *testing.T, s *store.Store, cutoff time.Time) map[store.ArticleID]bool {
	t.Helper()

	// Forgetting is what releases a claim, so it has to have run before anything is
	// expirable. That is the pipeline rather than a test convenience: a reader's own
	// window decides when they let go, and expiry only asks whether everybody has.
	//
	// Applied to every reader at the same cutoff here, which is the single-window
	// case these tests were written for; forget_integration_test.go covers windows
	// that differ.
	rows, err := s.Pool().Query(t.Context(), `SELECT id FROM users`)
	if err != nil {
		t.Fatalf("listing users: %v", err)
	}
	var readers []store.UserID
	for rows.Next() {
		var id store.UserID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scanning a user: %v", err)
		}
		readers = append(readers, id)
	}
	rows.Close()
	for _, id := range readers {
		if _, err := s.ForgetReadArticles(t.Context(), id, cutoff, 100); err != nil {
			t.Fatalf("ForgetReadArticles(%d) = %v", id, err)
		}
	}

	found, err := s.ExpirableArticles(t.Context(), 100)
	if err != nil {
		t.Fatalf("ExpirableArticles() = %v", err)
	}
	ids := make(map[store.ArticleID]bool, len(found))
	for _, e := range found {
		ids[e.ArticleID] = true
	}
	return ids
}

func TestReadArticlesBecomeExpirableAfterTheCutoff(t *testing.T) {
	tr := setupTwoReaders(t)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)

	// Not read at all: never expirable.
	if expirableIDs(t, tr.store, cutoff)[tr.aliceOnly] {
		t.Fatal("an unread article is expirable")
	}

	// Read, but recently.
	markRead(t, tr, tr.alice, tr.aliceOnly, time.Now().Add(-1*time.Hour))
	if expirableIDs(t, tr.store, cutoff)[tr.aliceOnly] {
		t.Error("an article read an hour ago is expirable under a seven day policy")
	}

	// Read, long enough ago.
	markRead(t, tr, tr.alice, tr.aliceOnly, time.Now().Add(-30*24*time.Hour))
	if !expirableIDs(t, tr.store, cutoff)[tr.aliceOnly] {
		t.Error("an article read a month ago is not expirable under a seven day policy")
	}
}

// The four protections, each on its own. Any one of them must be enough.
func TestProtectionsBlockExpiry(t *testing.T) {
	long := time.Now().Add(-30 * 24 * time.Hour)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)

	tests := []struct {
		name    string
		protect func(t *testing.T, tr twoReaders)

		// beforeSweep applies the protection before anything is forgotten, for the
		// ones that act at that stage rather than on expiry.
		beforeSweep bool
	}{
		{
			name: "starred",
			protect: func(t *testing.T, tr twoReaders) {
				if _, err := tr.store.SetStarred(t.Context(), tr.alice, tr.aliceOnly, true); err != nil {
					t.Fatalf("SetStarred() = %v", err)
				}
			},
		},
		{
			name: "kept",
			protect: func(t *testing.T, tr twoReaders) {
				if _, err := tr.store.SetKept(t.Context(), tr.alice, tr.aliceOnly, true); err != nil {
					t.Fatalf("SetKept() = %v", err)
				}
			},
		},
		{
			name: "saved by hand",
			protect: func(t *testing.T, tr twoReaders) {
				view, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
				if err != nil {
					t.Fatalf("ArticleForUser() = %v", err)
				}
				if _, err := tr.store.SaveArticle(t.Context(), tr.alice, view.Article.URLCanonical); err != nil {
					t.Fatalf("SaveArticle() = %v", err)
				}
			},
		},
		{
			name: "read but with no recorded time",
			// Applied before the sweep, because this protection now lives in
			// forgetting rather than in expiry: a row with no read time is never
			// forgotten, so it never reaches the point of having no claim. Under the
			// old single-cutoff design the same rule sat in ExpirableArticles, where
			// a NULL read_at meant "we do not know when" — it now also means "this
			// is a tombstone", which is why the check had to move to where the two
			// are still distinguishable.
			beforeSweep: true,
			protect: func(t *testing.T, tr twoReaders) {
				if _, err := tr.pool.Exec(t.Context(),
					`UPDATE article_state SET read_at = NULL WHERE article_id = $1`,
					tr.aliceOnly); err != nil {
					t.Fatalf("clearing read_at: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := setupTwoReaders(t)

			markRead(t, tr, tr.alice, tr.aliceOnly, long)

			if tt.beforeSweep {
				tt.protect(t, tr)
			} else {
				if !expirableIDs(t, tr.store, cutoff)[tr.aliceOnly] {
					t.Fatal("the article is not expirable before the protection is applied, " +
						"so this test proves nothing")
				}
				tt.protect(t, tr)
			}

			if expirableIDs(t, tr.store, cutoff)[tr.aliceOnly] {
				t.Errorf("an article protected by %q is still expirable", tt.name)
			}
		})
	}
}

// The one that matters most. article_content and assets are a shared pool, so
// one reader finishing with an article says nothing about whether it can go.
func TestAnotherReadersClaimBlocksExpiry(t *testing.T) {
	tr := setupTwoReaders(t)
	long := time.Now().Add(-30 * 24 * time.Hour)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)

	// The fixture's shared article is carried by both feeds and is one row, which
	// is exactly the situation this rule exists for.
	shared := tr.shared

	markRead(t, tr, tr.alice, shared, long)

	// Bob subscribes but has never opened it: unread by definition.
	if expirableIDs(t, tr.store, cutoff)[shared] {
		t.Error("an article one reader finished but another has never seen is expirable")
	}

	// Bob reads it, but recently.
	markRead(t, tr, tr.bob, shared, time.Now().Add(-1*time.Hour))
	if expirableIDs(t, tr.store, cutoff)[shared] {
		t.Error("an article the second reader read an hour ago is expirable")
	}

	// Bob stars it.
	markRead(t, tr, tr.bob, shared, long)
	if _, err := tr.store.SetStarred(t.Context(), tr.bob, shared, true); err != nil {
		t.Fatalf("SetStarred() = %v", err)
	}
	if expirableIDs(t, tr.store, cutoff)[shared] {
		t.Error("an article the second reader starred is expirable")
	}
}

// Both readers finished with it long ago and neither ever showed any interest:
// the only combination that releases a shared article.
func TestASharedArticleExpiresOnceEveryReaderIsDone(t *testing.T) {
	tr := setupTwoReaders(t)
	long := time.Now().Add(-30 * 24 * time.Hour)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)

	markRead(t, tr, tr.alice, tr.shared, long)
	markRead(t, tr, tr.bob, tr.shared, long)

	if !expirableIDs(t, tr.store, cutoff)[tr.shared] {
		t.Error("an article both readers finished long ago is not expirable")
	}
}

// Starring stamps saved_at, and unstarring deliberately leaves it — so an
// article that was ever starred is protected from expiry forever, even after the
// star is removed.
//
// That is the existing meaning of saved_at ("keep this reachable even if the
// feed goes away"), not something retention invented, and it fails safe. It is
// worth a test because it is genuinely surprising: a reader clearing stars to
// reclaim space will find that it reclaims nothing.
func TestUnstarringDoesNotReleaseAnArticle(t *testing.T) {
	tr := setupTwoReaders(t)
	long := time.Now().Add(-30 * 24 * time.Hour)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)

	markRead(t, tr, tr.alice, tr.aliceOnly, long)
	if !expirableIDs(t, tr.store, cutoff)[tr.aliceOnly] {
		t.Fatal("the article is not expirable to begin with")
	}

	for _, on := range []bool{true, false} {
		if _, err := tr.store.SetStarred(t.Context(), tr.alice, tr.aliceOnly, on); err != nil {
			t.Fatalf("SetStarred(%v) = %v", on, err)
		}
	}

	if expirableIDs(t, tr.store, cutoff)[tr.aliceOnly] {
		t.Error("an article that was starred and then unstarred became expirable; " +
			"saved_at is supposed to survive unstarring")
	}
}

func TestExpireArticleReleasesTheBodyAndKeepsTheRecord(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	before, err := tr.store.ArticleForUser(ctx, tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if !before.HasBody {
		t.Fatal("the fixture article has no body, so expiring it proves nothing")
	}

	result, err := tr.store.ExpireArticle(ctx, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ExpireArticle() = %v", err)
	}
	if result.BodyBytes <= 0 {
		t.Errorf("BodyBytes = %d, want the size of the body it deleted", result.BodyBytes)
	}

	after, err := tr.store.ArticleForUser(ctx, tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("the article is gone entirely after expiry: %v", err)
	}
	if after.HasBody {
		t.Error("the body survived expiry")
	}
	if after.ExpiredAt == nil {
		t.Error("the article does not record that it was expired, so the reader cannot be told why it is empty")
	}
	if after.Article.Title != before.Article.Title || after.Article.URLCanonical != before.Article.URLCanonical {
		t.Error("expiry lost the article's identity, so search and deduplication no longer know it existed")
	}
}

// Assets are content-addressed and shared. An image used by two articles must
// survive the expiry of one of them.
func TestExpiryKeepsAssetsStillUsedElsewhere(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	const sha = "shared-image-sha"
	if _, err := tr.pool.Exec(ctx, `
		INSERT INTO assets (sha256, media_type, byte_size, fs_path)
		VALUES ($1, 'image/avif', 1234, 'assets/sha256/sh/ar/shared.avif')`, sha); err != nil {
		t.Fatalf("creating the shared asset: %v", err)
	}
	for _, id := range []store.ArticleID{tr.aliceOnly, tr.bobOnly} {
		if _, err := tr.pool.Exec(ctx,
			`INSERT INTO article_assets (article_id, sha256) VALUES ($1, $2)`, id, sha); err != nil {
			t.Fatalf("linking the asset to article %d: %v", id, err)
		}
	}

	result, err := tr.store.ExpireArticle(ctx, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ExpireArticle() = %v", err)
	}

	for _, p := range result.AssetPaths {
		if p == "assets/sha256/sh/ar/shared.avif" {
			t.Error("expiry returned an asset still used by another article for deletion")
		}
	}

	var exists bool
	if err := tr.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM assets WHERE sha256 = $1)`, sha).Scan(&exists); err != nil {
		t.Fatalf("checking the asset: %v", err)
	}
	if !exists {
		t.Error("an asset still referenced by another article was deleted, silently breaking that article")
	}
}

func TestExpiryDeletesAssetsNothingElseUses(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	const sha = "lonely-image-sha"
	const path = "assets/sha256/lo/ne/lonely.avif"
	if _, err := tr.pool.Exec(ctx, `
		INSERT INTO assets (sha256, media_type, byte_size, fs_path)
		VALUES ($1, 'image/avif', 4321, $2)`, sha, path); err != nil {
		t.Fatalf("creating the asset: %v", err)
	}
	if _, err := tr.pool.Exec(ctx,
		`INSERT INTO article_assets (article_id, sha256) VALUES ($1, $2)`, tr.aliceOnly, sha); err != nil {
		t.Fatalf("linking the asset: %v", err)
	}

	result, err := tr.store.ExpireArticle(ctx, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ExpireArticle() = %v", err)
	}

	var reported bool
	for _, p := range result.AssetPaths {
		if p == path {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the orphaned asset was not reported for deletion, so its file leaks: %v", result.AssetPaths)
	}
	if result.AssetBytes != 4321 {
		t.Errorf("AssetBytes = %d, want 4321", result.AssetBytes)
	}

	var exists bool
	if err := tr.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM assets WHERE sha256 = $1)`, sha).Scan(&exists); err != nil {
		t.Fatalf("checking the asset: %v", err)
	}
	if exists {
		t.Error("the orphaned asset row survived expiry")
	}
}

// An expired article must not come back round for expiry again, or every run
// would rediscover the whole history.
func TestExpiredArticlesAreNotFoundAgain(t *testing.T) {
	tr := setupTwoReaders(t)
	long := time.Now().Add(-30 * 24 * time.Hour)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)

	markRead(t, tr, tr.alice, tr.aliceOnly, long)
	if !expirableIDs(t, tr.store, cutoff)[tr.aliceOnly] {
		t.Fatal("the article is not expirable to begin with")
	}

	if _, err := tr.store.ExpireArticle(t.Context(), tr.aliceOnly); err != nil {
		t.Fatalf("ExpireArticle() = %v", err)
	}

	if expirableIDs(t, tr.store, cutoff)[tr.aliceOnly] {
		t.Error("an already-expired article is offered for expiry again")
	}
}

// forgetEverybody runs the forgetting sweep for every reader at one cutoff, which
// is what has to happen before anything is expirable.
func forgetEverybody(t *testing.T, s *store.Store, cutoff time.Time) {
	t.Helper()
	expirableIDs(t, s, cutoff)
}
