package store_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// An empty category is the thing free text could not express, and the reason a
// reader could not make a folder and then move feeds into it.
func TestACategoryCanExistWithNoFeeds(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	id, err := s.CreateCategory(ctx, alice, "Reading later")
	if err != nil {
		t.Fatalf("CreateCategory() = %v", err)
	}
	if id == 0 {
		t.Fatal("CreateCategory() returned no id")
	}

	got, err := s.CategoryByName(ctx, alice, "Reading later")
	if err != nil {
		t.Fatalf("CategoryByName() = %v", err)
	}
	if got != id {
		t.Errorf("CategoryByName() = %d, want %d", got, id)
	}
}

// The names have to stay unique per reader, because every read is still keyed by
// name: two categories called "Tech" would each claim the same feeds.
func TestTwoCategoriesCannotShareAName(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	if _, err := s.CreateCategory(ctx, alice, "Tech"); err != nil {
		t.Fatalf("CreateCategory() = %v", err)
	}
	if _, err := s.CreateCategory(ctx, alice, "Tech"); !errors.Is(err, store.ErrCategoryExists) {
		t.Errorf("a duplicate name returned %v, want ErrCategoryExists", err)
	}
	// Whitespace is not a distinguishing feature of a name.
	if _, err := s.CreateCategory(ctx, alice, "  Tech  "); !errors.Is(err, store.ErrCategoryExists) {
		t.Errorf("a padded duplicate returned %v, want ErrCategoryExists", err)
	}
	if _, err := s.CreateCategory(ctx, alice, "   "); !errors.Is(err, store.ErrCategoryNameBlank) {
		t.Errorf("a blank name returned %v, want ErrCategoryNameBlank", err)
	}
}

// Renaming keeps the id, which is the whole reason this became a table: the Fever
// group id was a hash of the name, so renaming reshuffled a client's folders.
func TestRenamingACategoryKeepsItsIdentity(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	id, err := s.CreateCategory(ctx, alice, "Tech")
	if err != nil {
		t.Fatalf("CreateCategory() = %v", err)
	}
	if err := s.RenameCategory(ctx, alice, id, "Technology"); err != nil {
		t.Fatalf("RenameCategory() = %v", err)
	}

	got, err := s.CategoryByName(ctx, alice, "Technology")
	if err != nil {
		t.Fatalf("CategoryByName() after rename = %v", err)
	}
	if got != id {
		t.Errorf("the category's id changed on rename: %d then %d — a Fever client's cached folder would break", id, got)
	}

	// Changing only the case of a name has to be possible, which is why the unique
	// constraint is case-sensitive.
	if err := s.RenameCategory(ctx, alice, id, "technology"); err != nil {
		t.Errorf("renaming to a different case failed: %v", err)
	}
}

// Another reader's category is not found rather than forbidden, the rule every
// scoped read here follows: a distinct refusal confirms it exists.
func TestACategoryBelongingToSomebodyElseIsNotFound(t *testing.T) {
	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	var bob store.UserID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("creating the second reader: %v", err)
	}

	id, err := s.CreateCategory(ctx, bob, "Bob's folder")
	if err != nil {
		t.Fatalf("CreateCategory() = %v", err)
	}

	if err := s.RenameCategory(ctx, alice, id, "Mine now"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("renaming another reader's category returned %v, want not-found", err)
	}
	if _, err := s.DeleteCategory(ctx, alice, id, store.DispositionUncategorized, 0); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("deleting another reader's category returned %v, want not-found", err)
	}
	// And it is still there.
	if _, err := s.CategoryByName(ctx, bob, "Bob's folder"); err != nil {
		t.Errorf("the other reader's category was affected: %v", err)
	}
}

// seedFiled subscribes a feed under a category and gives it one article with a body,
// which is what makes the "no article was touched" assertions mean anything.
func seedFiled(t *testing.T, s *store.Store, userID store.UserID, category store.CategoryID, slug string) (store.FeedID, store.ArticleID) {
	t.Helper()
	ctx := t.Context()

	feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://example.com/" + slug + "/feed.xml",
		Title:   slug,
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if category != 0 {
		if err := s.SetFeedCategory(ctx, userID, feedID, &category); err != nil {
			t.Fatalf("SetFeedCategory() = %v", err)
		}
	}

	articleID, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://example.com/" + slug + "/post",
		Title:        slug + " post",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if _, err := s.InsertContent(ctx, store.ContentParams{
		ArticleID: articleID, ExtractorName: "trafilatura", ExtractorVersion: "7",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>The body of " + slug + ".</p>", Text: "The body of " + slug + ".",
		WordCount: 5,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}
	if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
		FeedID: feedID, ArticleID: articleID, GUID: "guid-" + slug,
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
	return feedID, articleID
}

func bodyStillThere(t *testing.T, s *store.Store, id store.ArticleID) bool {
	t.Helper()
	c, err := s.CurrentContent(t.Context(), id)
	return err == nil && c.HTML != ""
}

// **The assertion the whole delete-a-category decision rests on: no disposition
// touches an article.** Nothing in this project deletes one, an article has no
// category of its own — it is derived through feed_items to the feed that carried it
// — and the destructive intent is served by unsubscribing, which keeps everything it
// ever brought in.
//
// All three dispositions in one test, deliberately: the property is about the set of
// them, and a reader of this file should be able to see at a glance that none is an
// exception.
func TestNoDispositionEverTouchesAnArticle(t *testing.T) {
	for _, tc := range []struct {
		name            string
		how             store.CategoryDisposition
		wantSubscribed  bool
		wantUncategoriz bool
	}{
		{"leave uncategorized", store.DispositionUncategorized, true, true},
		{"move elsewhere", store.DispositionMove, true, false},
		{"unsubscribe the feeds", store.DispositionUnsubscribe, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s, alice := dbtest.SetupWithUser(t)
			ctx := t.Context()

			doomed, err := s.CreateCategory(ctx, alice, "Doomed")
			if err != nil {
				t.Fatalf("CreateCategory() = %v", err)
			}
			elsewhere, err := s.CreateCategory(ctx, alice, "Elsewhere")
			if err != nil {
				t.Fatalf("CreateCategory() = %v", err)
			}

			feedID, articleID := seedFiled(t, s, alice, doomed, "doomed-feed")

			into := store.CategoryID(0)
			if tc.how == store.DispositionMove {
				into = elsewhere
			}
			result, err := s.DeleteCategory(ctx, alice, doomed, tc.how, into)
			if err != nil {
				t.Fatalf("DeleteCategory(%s) = %v", tc.how, err)
			}
			if result.Feeds != 1 {
				t.Errorf("reported %d feeds affected, want 1", result.Feeds)
			}

			// The article, and its body, survive every branch.
			if _, err := s.GetArticle(ctx, articleID); err != nil {
				t.Errorf("the article is gone after %s: %v", tc.how, err)
			}
			if !bodyStillThere(t, s, articleID) {
				t.Errorf("the article's body is gone after %s", tc.how)
			}

			// And the category itself is.
			if _, err := s.CategoryByName(ctx, alice, "Doomed"); !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("the category survived %s: %v", tc.how, err)
			}

			feeds, err := s.ListFeeds(ctx, alice)
			if err != nil {
				t.Fatalf("ListFeeds() = %v", err)
			}
			var found bool
			for _, f := range feeds {
				if f.ID == feedID {
					found = true
				}
			}
			if found != tc.wantSubscribed {
				t.Errorf("feed still subscribed = %v after %s, want %v", found, tc.how, tc.wantSubscribed)
			}
			if tc.how == store.DispositionMove {
				if _, err := s.CategoryByName(ctx, alice, "Elsewhere"); err != nil {
					t.Errorf("the destination category is gone: %v", err)
				}
			}
		})
	}
}

// Refiling a category's feeds into itself would leave them uncategorized by accident
// rather than by choice, which is a different answer than the reader gave.
func TestACategoryCannotBeMovedIntoItself(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	id, err := s.CreateCategory(ctx, alice, "Tech")
	if err != nil {
		t.Fatalf("CreateCategory() = %v", err)
	}
	seedFiled(t, s, alice, id, "tech-feed")

	if _, err := s.DeleteCategory(ctx, alice, id, store.DispositionMove, id); err == nil {
		t.Error("moving a category's feeds into itself was allowed")
	}
	// And nothing happened.
	if _, err := s.CategoryByName(ctx, alice, "Tech"); err != nil {
		t.Errorf("the category was deleted by a refusal: %v", err)
	}
}

// The foreign key's delete rule *is* the delete-a-category decision, and it is the
// one part of it no behavioral test can reach: goose will not re-apply a migration to
// a database that already has it, so changing the rule in 00013 leaves an existing
// test database untouched. On a fresh database — which is what CI runs against — this
// is the check.
//
// CASCADE would delete the feeds, which is a separate deliberate act that already
// exists and already asks first. RESTRICT would make deleting a non-empty category
// impossible, which is the case a reader wants it for.
func TestTheCategoryForeignKeyLeavesFeedsAlone(t *testing.T) {
	pool, _, _ := dbtest.SetupWithUser(t)

	var rule string
	if err := pool.QueryRow(t.Context(), `
		SELECT rc.delete_rule
		FROM information_schema.referential_constraints rc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_name = rc.constraint_name
		WHERE kcu.table_name = 'feeds' AND kcu.column_name = 'category_id'`).Scan(&rule); err != nil {
		t.Fatalf("reading the delete rule for feeds.category_id: %v", err)
	}

	if rule != "SET NULL" {
		t.Errorf("feeds.category_id deletes with %q, want %q — %s",
			rule, "SET NULL",
			"CASCADE would delete subscriptions on a folder deletion, RESTRICT would refuse to delete a non-empty folder")
	}
}

// An empty category has to appear in the list, which is the thing free text could
// not do and the reason "create a folder, then move feeds into it" was impossible.
func TestAnEmptyCategoryIsListed(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	if _, err := s.CreateCategory(ctx, alice, "Empty"); err != nil {
		t.Fatalf("CreateCategory() = %v", err)
	}

	got, err := s.ListCategories(ctx, alice)
	if err != nil {
		t.Fatalf("ListCategories() = %v", err)
	}
	var found *store.Category
	for i := range got {
		if got[i].Name == "Empty" {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("a category with no feeds is invisible: %+v", got)
	}
	if found.Feeds != 0 || found.Unread != 0 {
		t.Errorf("the empty category reports %d feeds and %d unread, want 0 and 0", found.Feeds, found.Unread)
	}

	// And it is offerable in a picker, or it can never be filled.
	names, err := s.ListCategoryNames(ctx, alice)
	if err != nil {
		t.Fatalf("ListCategoryNames() = %v", err)
	}
	var offered bool
	for _, n := range names {
		if n == "Empty" {
			offered = true
		}
	}
	if !offered {
		t.Errorf("the empty category is not offered as a destination: %v", names)
	}
}

// A rename has to move the feeds' apparent folder without touching the feeds, and it
// has to be what every name-keyed read then sees. feeds.category is frozen at the
// 00013 backfill, so a read that forgot the join would still report the old name —
// which would look like the rename silently failing.
func TestARenameIsWhatEveryReadSees(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	id, err := s.CreateCategory(ctx, alice, "Tech")
	if err != nil {
		t.Fatalf("CreateCategory() = %v", err)
	}
	feedID, _ := seedFiled(t, s, alice, id, "tech-feed")

	if err := s.RenameCategory(ctx, alice, id, "Technology"); err != nil {
		t.Fatalf("RenameCategory() = %v", err)
	}

	feed, err := s.GetFeed(ctx, alice, feedID)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if feed.Category != "Technology" {
		t.Errorf("GetFeed reports category %q after the rename, want %q — a read without the join returns the frozen column",
			feed.Category, "Technology")
	}

	feeds, err := s.FeedsInCategory(ctx, alice, "Technology")
	if err != nil {
		t.Fatalf("FeedsInCategory() = %v", err)
	}
	if len(feeds) != 1 {
		t.Errorf("FeedsInCategory(%q) returned %d feeds, want 1", "Technology", len(feeds))
	}
	if old, err := s.FeedsInCategory(ctx, alice, "Tech"); err != nil || len(old) != 0 {
		t.Errorf("the old name still selects %d feeds (err %v), want none", len(old), err)
	}
}
