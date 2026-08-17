package store_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// These require a live PostgreSQL and skip without TOME_TEST_DATABASE_URL.

func newArticle(t *testing.T, s *store.Store, url string) store.ArticleID {
	t.Helper()

	id, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{URLCanonical: url})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	return id
}

func insertBody(t *testing.T, s *store.Store, id store.ArticleID, p store.ContentParams) bool {
	t.Helper()

	p.ArticleID = id
	if p.ExtractorVersion == "" {
		p.ExtractorVersion = "1"
	}
	if p.ContentOrigin == "" {
		p.ContentOrigin = store.OriginFetched
	}
	if p.Text == "" {
		p.Text = "body text"
	}
	if p.HTML == "" {
		p.HTML = "<p>body text</p>"
	}

	current, err := s.InsertContent(t.Context(), p)
	if err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}
	return current
}

// Extraction is versioned: a new body demotes the old one rather than
// destroying it, so a bad extractor release can be diagnosed afterwards.
func TestInsertContentDemotesRatherThanDeletes(t *testing.T) {
	pool, s, _ := dbtest.SetupWithUser(t)
	id := newArticle(t, s, "https://example.com/a")

	insertBody(t, s, id, store.ContentParams{
		ExtractorName: "readability", ExtractorVersion: "1", Text: "first extraction",
	})
	insertBody(t, s, id, store.ContentParams{
		ExtractorName: "trafilatura", ExtractorVersion: "2", Text: "second extraction",
	})

	current, err := s.CurrentContent(t.Context(), id)
	if err != nil {
		t.Fatalf("CurrentContent() = %v", err)
	}
	if current.ExtractorName != "trafilatura" {
		t.Errorf("current extractor = %q, want the newest", current.ExtractorName)
	}

	var total int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM article_content WHERE article_id = $1`, id).Scan(&total); err != nil {
		t.Fatalf("counting bodies: %v", err)
	}
	if total != 2 {
		t.Errorf("the article has %d bodies, want 2 — the old one must be kept", total)
	}

	// The partial unique index permits exactly one current body.
	var currentRows int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM article_content WHERE article_id = $1 AND is_current`, id).Scan(&currentRows); err != nil {
		t.Fatalf("counting current bodies: %v", err)
	}
	if currentRows != 1 {
		t.Errorf("%d bodies are marked current, want exactly 1", currentRows)
	}
}

// §2.3: an imported body may be the only surviving copy of a dead URL, so a
// later fetch is stored beside it rather than over it.
func TestImmutableContentIsNeverReplaced(t *testing.T) {
	pool, s, _ := dbtest.SetupWithUser(t)
	id := newArticle(t, s, "https://dead-site.example.com/only-copy")

	insertBody(t, s, id, store.ContentParams{
		ExtractorName: "imported",
		ContentOrigin: "import:wallabag",
		Immutable:     true,
		Text:          "the only surviving copy",
	})

	madeCurrent := insertBody(t, s, id, store.ContentParams{
		ExtractorName: "trafilatura", ExtractorVersion: "2", Text: "a later re-fetch",
	})
	if madeCurrent {
		t.Error("a fetched body replaced an immutable one")
	}

	current, err := s.CurrentContent(t.Context(), id)
	if err != nil {
		t.Fatalf("CurrentContent() = %v", err)
	}
	if current.Text != "the only surviving copy" {
		t.Errorf("current body = %q, want the immutable import to still be current", current.Text)
	}
	if !current.Immutable {
		t.Error("the current body is no longer marked immutable")
	}

	// The new body is kept, just not current: it can be promoted deliberately.
	var total int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM article_content WHERE article_id = $1`, id).Scan(&total); err != nil {
		t.Fatalf("counting bodies: %v", err)
	}
	if total != 2 {
		t.Errorf("the article has %d bodies, want the re-fetch kept alongside the import", total)
	}
}

// The M2 acceptance criterion: content rows flagged immutable are provably
// skipped by a bulk reprocess. Provable means excluded by the query.
func TestReextractCandidatesExcludeImmutable(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	mutable := newArticle(t, s, "https://example.com/mutable")
	imported := newArticle(t, s, "https://example.com/imported")
	unfetched := newArticle(t, s, "https://example.com/never-fetched")
	current := newArticle(t, s, "https://example.com/already-current")

	for _, id := range []store.ArticleID{mutable, imported, current} {
		if err := s.RecordFetchSuccess(ctx, id, "deadbeef", "articles/2026/08/x/raw.html.gz"); err != nil {
			t.Fatalf("RecordFetchSuccess() = %v", err)
		}
	}

	insertBody(t, s, mutable, store.ContentParams{ExtractorName: "readability", ExtractorVersion: "1"})
	insertBody(t, s, imported, store.ContentParams{
		ExtractorName: "imported", ContentOrigin: "import:wallabag",
		ExtractorVersion: "1", Immutable: true,
	})
	insertBody(t, s, unfetched, store.ContentParams{ExtractorName: "feed_body", ExtractorVersion: "1"})
	insertBody(t, s, current, store.ContentParams{ExtractorName: "trafilatura", ExtractorVersion: "2"})

	candidates, err := s.System().ReextractCandidates(ctx, "2", 0, 100)
	if err != nil {
		t.Fatalf("ReextractCandidates() = %v", err)
	}

	got := make(map[store.ArticleID]bool, len(candidates))
	for _, c := range candidates {
		got[c.ArticleID] = true
	}

	if !got[mutable] {
		t.Error("the out-of-date mutable article was not a candidate")
	}
	if got[imported] {
		t.Error("an immutable body was offered for reprocessing")
	}
	if got[current] {
		t.Error("a body already at the current version was offered for reprocessing")
	}
	if got[unfetched] {
		t.Error("an article with no stored page was offered for reprocessing — there is nothing to re-extract from")
	}
}

// Pagination is by id cursor. Queueing does not change a row's version, so an
// offset-free repeat would return the same rows forever.
func TestReextractCandidatesPaginate(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	const total = 5
	ids := make([]store.ArticleID, 0, total)
	for i := range total {
		id := newArticle(t, s, "https://example.com/article-"+string(rune('a'+i)))
		if err := s.RecordFetchSuccess(ctx, id, "sha", "path"); err != nil {
			t.Fatalf("RecordFetchSuccess() = %v", err)
		}
		insertBody(t, s, id, store.ContentParams{ExtractorName: "readability", ExtractorVersion: "1"})
		ids = append(ids, id)
	}

	var seen []store.ArticleID
	var cursor store.ArticleID
	for range total + 2 { // more passes than needed; the walk must terminate
		batch, err := s.System().ReextractCandidates(ctx, "2", cursor, 2)
		if err != nil {
			t.Fatalf("ReextractCandidates() = %v", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, c := range batch {
			seen = append(seen, c.ArticleID)
			cursor = c.ArticleID
		}
	}

	if len(seen) != total {
		t.Errorf("the walk visited %d articles, want %d — pagination did not advance", len(seen), total)
	}
	for i, id := range ids {
		if i < len(seen) && seen[i] != id {
			t.Errorf("visit %d was article %d, want %d (ascending id order)", i, seen[i], id)
		}
	}
}

// The fullest body wins: the same story is commonly truncated in one feed and
// complete in another, and only the complete one is worth keeping.
func TestFeedBodyForPrefersTheLongest(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	id := newArticle(t, s, "https://example.com/syndicated")

	for i, body := range []string{"short teaser", "a much longer and more complete body of the article"} {
		feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
			FeedURL: "https://feed-" + string(rune('a'+i)) + ".example.com/rss",
			Title:   "Feed",
		})
		if err != nil {
			t.Fatalf("UpsertFeed() = %v", err)
		}
		if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
			FeedID: feedID, ArticleID: id, GUID: "guid", Content: body,
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}
	}

	got, err := s.FeedBodyFor(ctx, id)
	if err != nil {
		t.Fatalf("FeedBodyFor() = %v", err)
	}
	if got != "a much longer and more complete body of the article" {
		t.Errorf("FeedBodyFor() = %q, want the longest body", got)
	}
}

// An article with no feed behind it — a manual save — is not an error.
func TestFeedBodyForArticleWithNoFeed(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	got, err := s.FeedBodyFor(t.Context(), newArticle(t, s, "https://example.com/manual"))
	if err != nil {
		t.Errorf("FeedBodyFor() = %v, want no error for an article with no feed", err)
	}
	if got != "" {
		t.Errorf("FeedBodyFor() = %q, want empty", got)
	}
}

func TestFetchStatusTransitions(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	id := newArticle(t, s, "https://example.com/a")

	article, err := s.GetArticle(ctx, id)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.FetchStatus != store.FetchPending {
		t.Errorf("a new article is %q, want %q", article.FetchStatus, store.FetchPending)
	}

	if err := s.RecordFetchSuccess(ctx, id, "abc123", "articles/2026/08/a-1234/raw.html.gz"); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}

	article, err = s.GetArticle(ctx, id)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.FetchStatus != store.FetchOK {
		t.Errorf("FetchStatus = %q, want %q", article.FetchStatus, store.FetchOK)
	}
	if article.RawBlobSHA != "abc123" || article.RawBlobPath == "" {
		t.Errorf("the blob reference was not recorded: sha=%q path=%q",
			article.RawBlobSHA, article.RawBlobPath)
	}

	// A site saying no through robots.txt is skipped, not failed. The
	// distinction is what keeps the failed-fetch queue meaningful.
	if err := s.RecordFetchFailure(ctx, id, store.FetchSkipped, "disallowed by robots.txt"); err != nil {
		t.Fatalf("RecordFetchFailure() = %v", err)
	}
	if article, err = s.GetArticle(ctx, id); err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.FetchStatus != store.FetchSkipped {
		t.Errorf("FetchStatus = %q, want %q", article.FetchStatus, store.FetchSkipped)
	}

	// A status outside the CHECK constraint is refused before it reaches the
	// database, with a message naming the value.
	if err := s.RecordFetchFailure(ctx, id, "nonsense", "x"); err == nil {
		t.Error("RecordFetchFailure() accepted an invalid status")
	}
}

func TestPendingFetch(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	pending := newArticle(t, s, "https://example.com/pending")
	fetched := newArticle(t, s, "https://example.com/fetched")

	if err := s.RecordFetchSuccess(ctx, fetched, "sha", "path"); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}

	ids, err := s.System().PendingFetch(ctx, 100)
	if err != nil {
		t.Fatalf("PendingFetch() = %v", err)
	}
	if len(ids) != 1 || ids[0] != pending {
		t.Errorf("PendingFetch() = %v, want only the pending article %d", ids, pending)
	}
}

// A rule for example.com covers blog.example.com, and a rule for the subdomain
// wins over the parent.
func TestDomainRuleInheritance(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()
	sys := s.System()

	if err := sys.UpsertDomainRule(ctx, store.DomainRule{
		Domain:          "example.com",
		ContentSelector: "article.parent",
		StripSelectors:  []string{".ads"},
		Notes:           "the parent rule",
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}
	if err := sys.UpsertDomainRule(ctx, store.DomainRule{
		Domain:          "blog.example.com",
		ContentSelector: "div.specific",
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	tests := []struct {
		host         string
		wantSelector string
		wantFound    bool
	}{
		{"example.com", "article.parent", true},
		{"www.example.com", "article.parent", true},         // inherited
		{"deep.nested.example.com", "article.parent", true}, // inherited
		{"blog.example.com", "div.specific", true},          // the more specific rule wins
		{"other.org", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			rule, err := sys.DomainRuleFor(ctx, tt.host)
			if !tt.wantFound {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Errorf("DomainRuleFor(%q) = %+v, %v; want no rows", tt.host, rule, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("DomainRuleFor(%q) = %v", tt.host, err)
			}
			if rule.ContentSelector != tt.wantSelector {
				t.Errorf("selector = %q, want %q", rule.ContentSelector, tt.wantSelector)
			}
		})
	}

	// Strip selectors survive the round trip through the text[] column.
	rule, err := sys.DomainRuleFor(ctx, "example.com")
	if err != nil {
		t.Fatalf("DomainRuleFor() = %v", err)
	}
	if len(rule.StripSelectors) != 1 || rule.StripSelectors[0] != ".ads" {
		t.Errorf("StripSelectors = %v, want [.ads]", rule.StripSelectors)
	}
}

func TestDomainRuleUpsertAndDelete(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()
	sys := s.System()

	if err := sys.UpsertDomainRule(ctx, store.DomainRule{
		Domain: "Example.COM", ContentSelector: "article", RateLimitRPS: 0.5,
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	// The domain is normalized, so the same site written two ways is one rule.
	if err := sys.UpsertDomainRule(ctx, store.DomainRule{
		Domain: "example.com", ContentSelector: "main",
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	rules, err := sys.ListDomainRules(ctx)
	if err != nil {
		t.Fatalf("ListDomainRules() = %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("there are %d rules, want 1: %+v", len(rules), rules)
	}
	if rules[0].ContentSelector != "main" {
		t.Errorf("selector = %q, want the replacement %q", rules[0].ContentSelector, "main")
	}

	removed, err := sys.DeleteDomainRule(ctx, "example.com")
	if err != nil {
		t.Fatalf("DeleteDomainRule() = %v", err)
	}
	if !removed {
		t.Error("DeleteDomainRule() reported nothing removed")
	}

	if removed, _ = sys.DeleteDomainRule(ctx, "example.com"); removed {
		t.Error("DeleteDomainRule() reported a removal the second time")
	}
}

func TestExtractionStats(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	for i, name := range []string{"trafilatura", "trafilatura", "readability"} {
		id := newArticle(t, s, "https://example.com/stats-"+string(rune('a'+i)))
		insertBody(t, s, id, store.ContentParams{ExtractorName: name, Text: "some body text here"})
	}

	stats, err := s.System().ExtractionStats(ctx)
	if err != nil {
		t.Fatalf("ExtractionStats() = %v", err)
	}

	counts := make(map[string]int64, len(stats))
	for _, st := range stats {
		counts[st.Extractor] = st.Articles
	}
	if counts["trafilatura"] != 2 || counts["readability"] != 1 {
		t.Errorf("counts = %v, want trafilatura 2 and readability 1", counts)
	}
}
