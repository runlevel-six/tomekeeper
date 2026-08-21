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

// The imported-content-is-immutable principle: an imported body may be the only surviving copy of a dead URL, so a
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

// The acceptance criterion: content rows flagged immutable are provably
// skipped by a bulk reprocess. Provable means excluded by the query.
func TestReextractCandidatesExcludeImmutable(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	mutable := newArticle(t, s, "https://example.com/mutable")
	imported := newArticle(t, s, "https://example.com/imported")
	unfetched := newArticle(t, s, "https://example.com/never-fetched")
	current := newArticle(t, s, "https://example.com/already-current")

	for _, id := range []store.ArticleID{mutable, imported, current} {
		if err := s.RecordFetchSuccess(ctx, id, store.FetchedPage{SHA: "deadbeef", Path: "articles/2026/08/x/raw.html.gz"}); err != nil {
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

	candidates, err := s.System().ReextractCandidates(ctx, "2", "", 0, 100)
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
		if err := s.RecordFetchSuccess(ctx, id, store.FetchedPage{SHA: "sha", Path: "path"}); err != nil {
			t.Fatalf("RecordFetchSuccess() = %v", err)
		}
		insertBody(t, s, id, store.ContentParams{ExtractorName: "readability", ExtractorVersion: "1"})
		ids = append(ids, id)
	}

	var seen []store.ArticleID
	var cursor store.ArticleID
	for range total + 2 { // more passes than needed; the walk must terminate
		batch, err := s.System().ReextractCandidates(ctx, "2", "", cursor, 2)
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

	if err := s.RecordFetchSuccess(ctx, id, store.FetchedPage{SHA: "abc123", Path: "articles/2026/08/a-1234/raw.html.gz"}); err != nil {
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

// An article the pipeline gave up on must not be left claiming its images are
// still coming.
//
// The asset scheduler finds work by joining the current content row, so an
// article that never got a body is invisible to it forever. Against a real feed
// list that stranded 346 of 1,365 articles at 'pending' — a terminal state
// wearing a transient label, which any "images outstanding" count would report
// as work in progress until someone went looking.
func TestFailedFetchSettlesAssetsStatus(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	bodyless := newArticle(t, s, "https://example.com/comic-strip")
	if err := s.RecordFetchFailure(ctx, bodyless, store.FetchFailed, "extraction produced no content"); err != nil {
		t.Fatalf("RecordFetchFailure() = %v", err)
	}
	article, err := s.GetArticle(ctx, bodyless)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.AssetsStatus != store.AssetsNone {
		t.Errorf("AssetsStatus = %q for an article with no body, want %q — nothing will ever localize it",
			article.AssetsStatus, store.AssetsNone)
	}

	// The other half of the invariant: an article that *does* have localized
	// images keeps them when a later re-fetch fails. Clobbering this back to
	// 'none' would report an archived article as having no images at all.
	withAssets := newArticle(t, s, "https://example.com/real-article")
	insertBody(t, s, withAssets, store.ContentParams{
		ExtractorName:    "trafilatura",
		ExtractorVersion: "2",
		ContentOrigin:    store.OriginFetched,
		HTML:             "<p>A body long enough to be an article.</p>",
		Text:             "A body long enough to be an article.",
		WordCount:        7,
	})
	if err := s.SetAssetsStatus(ctx, withAssets, store.AssetsOK); err != nil {
		t.Fatalf("SetAssetsStatus() = %v", err)
	}
	if err := s.RecordFetchFailure(ctx, withAssets, store.FetchFailed, "HTTP 503 on a later re-fetch"); err != nil {
		t.Fatalf("RecordFetchFailure() = %v", err)
	}
	if article, err = s.GetArticle(ctx, withAssets); err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.AssetsStatus != store.AssetsOK {
		t.Errorf("AssetsStatus = %q after a failed re-fetch, want %q kept — the images are still there",
			article.AssetsStatus, store.AssetsOK)
	}
}

func TestPendingFetch(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	pending := newArticle(t, s, "https://example.com/pending")
	fetched := newArticle(t, s, "https://example.com/fetched")

	if err := s.RecordFetchSuccess(ctx, fetched, store.FetchedPage{SHA: "sha", Path: "path"}); err != nil {
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

// --domain scopes a reprocess to one site, which is the common case: a domain
// rule was just written and only that site's articles need re-extracting.
//
// The subdomain behavior has to match how a domain rule applies. A rule written
// for example.com governs blog.example.com, so a reprocess scoped to example.com
// must cover the same set, or the flag quietly does less than the rule it exists
// to apply.
func TestReextractCandidatesByDomain(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	// The last two are the reason this compares hosts rather than doing a LIKE
	// over the whole URL: `LIKE '%example.com%'` matches both, and the second is
	// a path an attacker controls.
	urls := map[string]string{
		"apex":         "https://example.com/a",
		"subdomain":    "https://blog.example.com/b",
		"deep":         "https://a.b.example.com/c",
		"with port":    "https://example.com:8443/d",
		"other host":   "https://other.example.org/e",
		"suffix trap":  "https://notexample.com/f",
		"in the query": "https://evil.com/g?ref=example.com",
	}

	ids := make(map[string]store.ArticleID, len(urls))
	for name, url := range urls {
		id := newArticle(t, s, url)
		if err := s.RecordFetchSuccess(ctx, id, store.FetchedPage{SHA: "sha-" + name, Path: "path/" + name}); err != nil {
			t.Fatalf("RecordFetchSuccess(%s) = %v", name, err)
		}
		insertBody(t, s, id, store.ContentParams{ExtractorName: "readability", ExtractorVersion: "1"})
		ids[name] = id
	}

	selected := func(domain string) map[store.ArticleID]bool {
		t.Helper()
		got, err := s.System().ReextractCandidates(ctx, "2", domain, 0, 100)
		if err != nil {
			t.Fatalf("ReextractCandidates(%q) = %v", domain, err)
		}
		out := make(map[store.ArticleID]bool, len(got))
		for _, c := range got {
			out[c.ArticleID] = true
		}
		return out
	}

	all := selected("")
	if len(all) != len(urls) {
		t.Errorf("an empty domain selected %d articles, want all %d", len(all), len(urls))
	}

	scoped := selected("example.com")

	for _, name := range []string{"apex", "subdomain", "deep", "with port"} {
		if !scoped[ids[name]] {
			t.Errorf("the %s article was not selected for example.com", name)
		}
	}
	for _, name := range []string{"other host", "suffix trap", "in the query"} {
		if scoped[ids[name]] {
			t.Errorf("the %q article was selected for example.com, which it does not belong to", name)
		}
	}

	// A subdomain scope must not reach up to the apex.
	sub := selected("blog.example.com")
	if !sub[ids["subdomain"]] {
		t.Error("blog.example.com did not select its own article")
	}
	if sub[ids["apex"]] {
		t.Error("blog.example.com selected the apex domain's article")
	}

	// Case and a trailing dot are both things a person types.
	for _, spelling := range []string{"EXAMPLE.COM", "example.com.", "  example.com  "} {
		if got := selected(spelling); len(got) != len(scoped) {
			t.Errorf("%q selected %d articles, want the same %d as the canonical spelling",
				spelling, len(got), len(scoped))
		}
	}

	// A host with nothing archived is not an error, just empty — the command says
	// so distinctly, because it is usually a typo.
	if got := selected("nothing-here.example.net"); len(got) != 0 {
		t.Errorf("an unarchived host selected %d articles, want 0", len(got))
	}
}

// An article whose extraction produced nothing is a candidate for reprocessing.
//
// This is the case reprocessing could not see until 2026-08-21, and the reason it
// mattered: the whole purpose of the command is applying an extraction improvement to the
// archive, and an article with no body is the one an improvement is most likely to
// rescue. On the maintainer's archive that was 343 articles, 280 of them webcomics the
// image rung would have archived three versions earlier.
func TestReextractCandidatesIncludeArticlesWithNoBody(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	// Never extracted at all: extract_attempt_version is NULL, which has to compare as
	// out of date. `<>` would not — NULL <> '5' is NULL — and that single operator is the
	// difference between reaching every article in the archive and reaching none of them.
	never := newArticle(t, s, "https://example.com/never-extracted")
	if err := s.RecordFetchSuccess(ctx, never, store.FetchedPage{
		SHA: "sha-never", Path: "articles/2026/08/never/raw.html.gz"}); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}

	// Attempted by an older extractor and produced nothing.
	stale := newArticle(t, s, "https://example.com/failed-under-an-old-extractor")
	if err := s.RecordFetchSuccess(ctx, stale, store.FetchedPage{
		SHA: "sha-stale", Path: "articles/2026/08/stale/raw.html.gz"}); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}
	if err := s.RecordExtractAttempt(ctx, stale, "3", 240); err != nil {
		t.Fatalf("RecordExtractAttempt() = %v", err)
	}

	// Attempted by the *current* extractor and produced nothing. Already up to date, so
	// reprocessing it would be work with a known answer — and a bare `tome reextract`
	// that kept picking it up would never be idempotent.
	current := newArticle(t, s, "https://example.com/failed-under-the-current-one")
	if err := s.RecordFetchSuccess(ctx, current, store.FetchedPage{
		SHA: "sha-current", Path: "articles/2026/08/current/raw.html.gz"}); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}
	if err := s.RecordExtractAttempt(ctx, current, "5", 240); err != nil {
		t.Fatalf("RecordExtractAttempt() = %v", err)
	}

	// No stored page: nothing to extract from, so not a candidate however out of date.
	nopage := newArticle(t, s, "https://example.com/never-fetched")

	got, err := s.System().ReextractCandidates(ctx, "5", "", 0, 100)
	if err != nil {
		t.Fatalf("ReextractCandidates() = %v", err)
	}

	selected := make(map[store.ArticleID]bool, len(got))
	for _, c := range got {
		selected[c.ArticleID] = true
	}

	if !selected[never] {
		t.Error("an article that has never been extracted is not a candidate; a NULL " +
			"attempt version has to compare as out of date")
	}
	if !selected[stale] {
		t.Error("an article whose extraction failed under an older version is not a candidate")
	}
	if selected[current] {
		t.Error("an article already attempted at the target version is a candidate, so a " +
			"bare reextract would never settle")
	}
	if selected[nopage] {
		t.Error("an article with no stored page is a candidate, and there is nothing to " +
			"extract from")
	}
}

// A rule that rescues an article from a page already on disk has to take the
// failure back with it, or the attention queue keeps listing work that is done.
//
// Both guards get their own case, because the interesting half is what this
// must *not* clear: an imported body whose page fetch really did fail is still a
// gap in the archive, and the queue is where a reader learns that.
func TestSuccessfulExtractionRetiresTheFailureItRecorded(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	body := store.ContentParams{
		ExtractorName:    "domain_rule",
		ExtractorVersion: "5",
		ContentOrigin:    store.OriginFetched,
		HTML:             `<p>The strip.</p><img src="https://example.com/strip.png">`,
		Text:             "The strip.",
		WordCount:        2,
	}

	// The rescued article: fetched fine, extracted to nothing, then a rule found
	// the body in the page that was already stored.
	rescued := newArticle(t, s, "https://comics.example.com/2026/strip")
	if err := s.RecordFetchSuccess(ctx, rescued, store.FetchedPage{SHA: "sha-rescued", Path: "p/raw.html.gz"}); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}
	if err := s.RecordFetchFailure(ctx, rescued, store.FetchFailed, "extraction produced no content"); err != nil {
		t.Fatalf("RecordFetchFailure() = %v", err)
	}
	insertBody(t, s, rescued, body)
	if err := s.ClearExtractionFailure(ctx, rescued); err != nil {
		t.Fatalf("ClearExtractionFailure() = %v", err)
	}
	article, err := s.GetArticle(ctx, rescued)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.FetchStatus != store.FetchOK {
		t.Errorf("FetchStatus = %q for an article whose page is on disk and whose body arrived, want %q",
			article.FetchStatus, store.FetchOK)
	}
	if article.FetchError != "" {
		t.Errorf("FetchError = %q, want it cleared — the reason no longer describes anything", article.FetchError)
	}

	// The article whose page never landed: a body exists, but it came from an
	// import, and the archive is still missing the page. The failure stands.
	imported := newArticle(t, s, "https://example.com/dead-url")
	if err := s.RecordFetchFailure(ctx, imported, store.FetchFailed, "HTTP 404"); err != nil {
		t.Fatalf("RecordFetchFailure() = %v", err)
	}
	insertBody(t, s, imported, body)
	if err := s.ClearExtractionFailure(ctx, imported); err != nil {
		t.Fatalf("ClearExtractionFailure() = %v", err)
	}
	if article, err = s.GetArticle(ctx, imported); err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.FetchStatus != store.FetchFailed {
		t.Errorf("FetchStatus = %q for an article with no stored page, want %q kept — that page is genuinely missing",
			article.FetchStatus, store.FetchFailed)
	}
	if article.FetchError != "HTTP 404" {
		t.Errorf("FetchError = %q, want the original reason kept", article.FetchError)
	}

	// A robots.txt refusal is about the fetch, not the extraction, so 'skipped'
	// is never something this may promote. Given a page as well, which is the
	// state a site that adds a Disallow after we already archived it produces —
	// without one, the stored-page guard above would carry this case and the
	// status clause would be along for the ride.
	skipped := newArticle(t, s, "https://example.com/disallowed")
	if err := s.RecordFetchSuccess(ctx, skipped, store.FetchedPage{SHA: "sha-skipped", Path: "q/raw.html.gz"}); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}
	if err := s.RecordFetchFailure(ctx, skipped, store.FetchSkipped, "disallowed by robots.txt"); err != nil {
		t.Fatalf("RecordFetchFailure() = %v", err)
	}
	if err := s.ClearExtractionFailure(ctx, skipped); err != nil {
		t.Fatalf("ClearExtractionFailure() = %v", err)
	}
	if article, err = s.GetArticle(ctx, skipped); err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.FetchStatus != store.FetchSkipped {
		t.Errorf("FetchStatus = %q, want %q kept", article.FetchStatus, store.FetchSkipped)
	}
}
