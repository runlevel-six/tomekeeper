package jobs

import (
	"bytes"
	"compress/gzip"
	"log/slog"
	"strings"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// A page with two plausible article bodies in it, so a selector decides which one
// a reader gets. Both are long enough to clear the extractor's floors.
var twoBodyPage = `<!DOCTYPE html><html lang="en"><head><title>Two Bodies</title></head><body>
<article class="main">
<h1>Two Bodies</h1>
<p>` + longProse("The main article body runs here and says a great deal about alpacas.") + `</p>
</article>
<aside class="sidebar">
<p>` + longProse("The sidebar instead discusses the migratory habits of arctic terns.") + `</p>
</aside>
</body></html>`

func longProse(seed string) string {
	return strings.TrimSpace(strings.Repeat(seed+" ", 40))
}

// The keystone of tenancy: one stored page, two readers, two rules, two bodies.
//
// Work is called directly rather than through the queue, following this package's
// established reason — what is being proved is the effect on two rows, and River's
// retry would otherwise decide how many times it happened.
func TestAReadersRuleProducesTheirOwnBody(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()
	system := s.System()

	bob, err := system.CreateUser(ctx, "bob", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	const path = "articles/test/two-bodies/raw.html.gz"
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(twoBodyPage)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := blobs.Put(ctx, path, bytes.NewReader(gz.Bytes())); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	const pageURL = "https://twobodies.example/story"
	articleID, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: pageURL, URLOriginal: pageURL, Title: "Two Bodies",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if err := s.RecordFetchSuccess(ctx, articleID,
		store.FetchedPage{SHA: "sha-two-bodies", Path: path}); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}

	// Both readers can see it — one shared article, reached through each of their
	// own subscriptions. That is what makes the assertions below about *bodies*
	// rather than about visibility.
	subscribe(t, s, alice, articleID, "https://twobodies.example/alice.xml")
	subscribe(t, s, bob, articleID, "https://twobodies.example/bob.xml")

	worker := &ExtractArticleWorker{
		store:     s,
		blobs:     blobs,
		extractor: extract.New(),
		log:       slog.New(slog.DiscardHandler),
	}
	run := func(t *testing.T, reader store.UserID) {
		t.Helper()
		err := worker.Work(ctx, &river.Job[ExtractArticleArgs]{
			JobRow: &rivertype.JobRow{},
			Args:   ExtractArticleArgs{ArticleID: int64(articleID), UserID: int64(reader)},
		})
		// No river client in this context, so the tail that enqueues localization
		// reports itself. Everything under test has already happened by then.
		if err != nil && !strings.Contains(err.Error(), "no river client") {
			t.Fatalf("Work(reader %d) = %v", reader, err)
		}
	}

	// The household's extraction first: what everyone gets.
	run(t, store.HouseholdRule)

	household, err := s.CurrentContent(ctx, articleID, store.Household())
	if err != nil {
		t.Fatalf("CurrentContent(household) = %v", err)
	}
	if !strings.Contains(household.Text, "alpacas") {
		t.Fatalf("the household body is not the main article: %.120q", household.Text)
	}
	householdTitle := articleTitle(t, s, articleID)

	// Alice writes a rule choosing the sidebar instead — a deliberate choice
	// nobody else made.
	if err := system.UpsertReaderRule(ctx, alice, store.DomainRule{
		Domain: "twobodies.example", ContentSelector: "aside.sidebar",
	}); err != nil {
		t.Fatalf("UpsertReaderRule() = %v", err)
	}
	run(t, alice)

	mine, err := s.CurrentContent(ctx, articleID, store.Owned(alice))
	if err != nil {
		t.Fatalf("CurrentContent(alice) = %v", err)
	}
	if !strings.Contains(mine.Text, "arctic terns") {
		t.Errorf("alice's body is not what her rule selected: %.120q", mine.Text)
	}
	if mine.RulesetKey == "" {
		t.Error("alice's body records no ruleset key, so nothing can tell it is hers")
	}

	// The household's is untouched, and so is Bob's view of it.
	after, err := s.CurrentContent(ctx, articleID, store.Household())
	if err != nil {
		t.Fatalf("CurrentContent(household) after = %v", err)
	}
	if after.Text != household.Text {
		t.Error("alice's extraction changed the household's body")
	}
	if got := bodyReadBy(t, s, bob, articleID); !strings.Contains(got, "alpacas") {
		t.Errorf("bob reads %.80q, want the household's body", got)
	}
	if got := bodyReadBy(t, s, alice, articleID); !strings.Contains(got, "arctic terns") {
		t.Errorf("alice reads %.80q, want her own", got)
	}

	// Article-level facts are shared, so a reader's run must not have written any.
	// A selector picking a different block usually picks a different heading with
	// it, and one reader's rule renaming an article in everybody's list is the
	// failure this guards.
	if now := articleTitle(t, s, articleID); now != householdTitle {
		t.Errorf("the shared title changed to %q after a reader's extraction", now)
	}
	article, err := s.GetArticle(ctx, articleID)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.ExtractAttemptVersion != extract.Version {
		t.Errorf("extract_attempt_version = %q, want the household's %q — a reader's run overwrote it",
			article.ExtractAttemptVersion, extract.Version)
	}
}

// Running the same extraction twice is a no-op the second time, per slot and per
// ruleset. Without this the backstop sweep would re-extract the whole archive on
// every pass.
func TestExtractionSkipsWhatIsAlreadyCurrent(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}
	const path = "articles/test/skip/raw.html.gz"
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(twoBodyPage)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := blobs.Put(ctx, path, bytes.NewReader(gz.Bytes())); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	const pageURL = "https://skip.example/story"
	articleID, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: pageURL, URLOriginal: pageURL,
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if err := s.RecordFetchSuccess(ctx, articleID,
		store.FetchedPage{SHA: "sha-skip", Path: path}); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}

	worker := &ExtractArticleWorker{
		store: s, blobs: blobs, extractor: extract.New(), log: slog.New(slog.DiscardHandler),
	}
	run := func(reader store.UserID) {
		err := worker.Work(ctx, &river.Job[ExtractArticleArgs]{
			JobRow: &rivertype.JobRow{},
			Args:   ExtractArticleArgs{ArticleID: int64(articleID), UserID: int64(reader)},
		})
		if err != nil && !strings.Contains(err.Error(), "no river client") {
			t.Fatalf("Work() = %v", err)
		}
	}
	count := func() int {
		var n int
		if err := s.Pool().QueryRow(ctx,
			`SELECT count(*) FROM article_content WHERE article_id = $1`, articleID).Scan(&n); err != nil {
			t.Fatalf("counting bodies: %v", err)
		}
		return n
	}

	run(store.HouseholdRule)
	first := count()
	run(store.HouseholdRule)
	if second := count(); second != first {
		t.Errorf("re-running the same extraction stored another body: %d then %d", first, second)
	}

	// A rule change is a different ruleset, so the same article is no longer
	// current for that reader and extracts again. This is what makes a sweep able
	// to find work by comparing keys rather than by being told.
	if err := s.System().UpsertReaderRule(ctx, alice, store.DomainRule{
		Domain: "skip.example", ContentSelector: "aside.sidebar",
	}); err != nil {
		t.Fatalf("UpsertReaderRule() = %v", err)
	}
	run(alice)
	if withFork := count(); withFork != first+1 {
		t.Errorf("a reader's rule produced %d bodies, want one more than %d", withFork, first)
	}
	run(alice)
	if again := count(); again != first+1 {
		t.Errorf("re-running the reader's extraction stored another body: %d", again)
	}

	// And editing the rule makes it stale again, which is the half a
	// version-only check misses — the version has not moved, so nothing else
	// distinguishes the body she has from the one her new rule would produce.
	// Found by neutering: without the ruleset comparison every assertion above
	// still passed.
	beforeEdit, err := s.CurrentContent(ctx, articleID, store.Owned(alice))
	if err != nil {
		t.Fatalf("CurrentContent(alice) = %v", err)
	}
	if err := s.System().UpsertReaderRule(ctx, alice, store.DomainRule{
		Domain: "skip.example", ContentSelector: "article.main",
	}); err != nil {
		t.Fatalf("UpsertReaderRule() = %v", err)
	}
	run(alice)

	afterEdit, err := s.CurrentContent(ctx, articleID, store.Owned(alice))
	if err != nil {
		t.Fatalf("CurrentContent(alice) after the edit = %v", err)
	}
	if afterEdit.Text == beforeEdit.Text {
		t.Error("editing a rule left the reader's body unchanged; the ruleset is not being compared")
	}
	if afterEdit.RulesetKey == beforeEdit.RulesetKey {
		t.Error("the stored ruleset key did not change with the rule")
	}
}

// A reader extracting successfully does not clear a failure recorded against the
// article, because the failure is the household's and is still true for everybody
// who has no rule.
func TestAReadersSuccessDoesNotClearTheArticlesFailure(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}
	const path = "articles/test/still-failed/raw.html.gz"
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(twoBodyPage)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := blobs.Put(ctx, path, bytes.NewReader(gz.Bytes())); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	const pageURL = "https://stillfailed.example/story"
	articleID, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: pageURL, URLOriginal: pageURL,
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if err := s.RecordFetchSuccess(ctx, articleID,
		store.FetchedPage{SHA: "sha-still-failed", Path: path}); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}
	// The household could not extract this page — the state the attention queue
	// exists to list.
	if err := s.RecordFetchFailure(ctx, articleID, store.FetchFailed,
		"extraction produced no content"); err != nil {
		t.Fatalf("RecordFetchFailure() = %v", err)
	}

	if err := s.System().UpsertReaderRule(ctx, alice, store.DomainRule{
		Domain: "stillfailed.example", ContentSelector: "aside.sidebar",
	}); err != nil {
		t.Fatalf("UpsertReaderRule() = %v", err)
	}

	worker := &ExtractArticleWorker{
		store: s, blobs: blobs, extractor: extract.New(), log: slog.New(slog.DiscardHandler),
	}
	if err := worker.Work(ctx, &river.Job[ExtractArticleArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   ExtractArticleArgs{ArticleID: int64(articleID), UserID: int64(alice)},
	}); err != nil && !strings.Contains(err.Error(), "no river client") {
		t.Fatalf("Work() = %v", err)
	}

	if _, err := s.CurrentContent(ctx, articleID, store.Owned(alice)); err != nil {
		t.Fatalf("alice's extraction did not produce a body: %v", err)
	}

	article, err := s.GetArticle(ctx, articleID)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.FetchStatus != store.FetchFailed {
		t.Errorf("fetch_status = %q after one reader succeeded, want it still %q — "+
			"the article is still bodyless for everybody without a rule",
			article.FetchStatus, store.FetchFailed)
	}
}

func articleTitle(t *testing.T, s *store.Store, id store.ArticleID) string {
	t.Helper()
	a, err := s.GetArticle(t.Context(), id)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	return a.Title
}

// subscribe gives a reader sight of an article by subscribing them to a feed that
// carries it — the ordinary way an article becomes visible.
//
// Not by starring it: state writes are themselves guarded by the visibility
// predicate, precisely so a reader cannot confirm what exists one insert at a time,
// so starring cannot bootstrap the visibility it depends on.
func subscribe(t *testing.T, s *store.Store, reader store.UserID, id store.ArticleID, feedURL string) {
	t.Helper()

	feedID, _, err := s.UpsertFeed(t.Context(), reader, store.FeedParams{
		FeedURL: feedURL, Title: "Example",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if _, err := s.InsertFeedItem(t.Context(), reader, store.FeedItemParams{
		FeedID: feedID, ArticleID: id, GUID: feedURL, Title: "Two Bodies",
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
}

func bodyReadBy(t *testing.T, s *store.Store, reader store.UserID, id store.ArticleID) string {
	t.Helper()
	// Through the reading path, which is what a reader actually gets, rather than
	// through CurrentContent, which asks about one slot.
	view, err := s.ArticleForUser(t.Context(), reader, id)
	if err != nil {
		t.Fatalf("ArticleForUser(%d) = %v", reader, err)
	}
	return view.Content.Text
}

// A reader's extraction writes nothing that belongs to the article.
//
// Split from the test above because that one could not prove it: the article there
// already had a title, and UpdateArticleMetadata fills gaps only, so the guard
// could be deleted and nothing would change. Neutering found that. Here the
// article starts with *no* title and no attempt recorded, and only a reader's
// extraction ever runs — so anything article-level that ends up filled in was
// filled in by them.
func TestAReadersExtractionWritesNothingArticleLevel(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}
	const path = "articles/test/article-level/raw.html.gz"
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(twoBodyPage)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := blobs.Put(ctx, path, bytes.NewReader(gz.Bytes())); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	// No title: a gap the page could fill, which is what makes the assertion sharp.
	const pageURL = "https://articlelevel.example/story"
	articleID, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: pageURL, URLOriginal: pageURL,
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if err := s.RecordFetchSuccess(ctx, articleID,
		store.FetchedPage{SHA: "sha-article-level", Path: path}); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}
	if err := s.System().UpsertReaderRule(ctx, alice, store.DomainRule{
		Domain: "articlelevel.example", ContentSelector: "aside.sidebar",
	}); err != nil {
		t.Fatalf("UpsertReaderRule() = %v", err)
	}

	before, err := s.GetArticle(ctx, articleID)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}

	worker := &ExtractArticleWorker{
		store: s, blobs: blobs, extractor: extract.New(), log: slog.New(slog.DiscardHandler),
	}
	// Alice's extraction, and nobody else's.
	if err := worker.Work(ctx, &river.Job[ExtractArticleArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   ExtractArticleArgs{ArticleID: int64(articleID), UserID: int64(alice)},
	}); err != nil && !strings.Contains(err.Error(), "no river client") {
		t.Fatalf("Work() = %v", err)
	}

	// She got a body.
	mine, err := s.CurrentContent(ctx, articleID, store.Owned(alice))
	if err != nil {
		t.Fatalf("CurrentContent(alice) = %v", err)
	}
	if !strings.Contains(mine.Text, "arctic terns") {
		t.Fatalf("alice's extraction did not run: %.80q", mine.Text)
	}

	after, err := s.GetArticle(ctx, articleID)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}

	// The title stayed a gap. One reader's selector naming an article for
	// everybody is the failure this guards, and it is invisible afterwards.
	if after.Title != before.Title {
		t.Errorf("a reader's extraction set the shared title to %q", after.Title)
	}
	// And the household still has not attempted this article. The re-extraction
	// sweep reads extract_attempt_version to find bodyless articles; a reader's
	// attempt recorded here tells it the household has tried when it has not, and
	// the article silently stops being a candidate.
	if after.ExtractAttemptVersion != before.ExtractAttemptVersion {
		t.Errorf("a reader's extraction recorded the household's attempt as %q",
			after.ExtractAttemptVersion)
	}
	// The household's slot is still empty, which is the whole claim.
	if _, err := s.CurrentContent(ctx, articleID, store.Household()); err == nil {
		t.Error("a reader's extraction produced a household body")
	}
}
