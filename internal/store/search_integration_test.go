package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// article adds one article with a body, on the given feed.
func searchable(t *testing.T, s *store.Store, userID store.UserID, feedID store.FeedID,
	slug, title, body string,
) store.ArticleID {
	t.Helper()
	ctx := t.Context()

	id, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://example.com/" + slug,
		Title:        title,
		SiteName:     "Example Journal",
	})
	if err != nil {
		t.Fatalf("UpsertArticle(%s) = %v", slug, err)
	}
	insertBody(t, s, id, store.ContentParams{Text: body, WordCount: len(strings.Fields(body))})

	if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
		FeedID: feedID, ArticleID: id, GUID: "guid-" + slug,
	}); err != nil {
		t.Fatalf("InsertFeedItem(%s) = %v", slug, err)
	}
	return id
}

func searchFixture(t *testing.T) (*store.Store, store.UserID, store.FeedID) {
	t.Helper()

	_, s, userID := dbtest.SetupWithUser(t)
	feedID, _, err := s.UpsertFeed(t.Context(), userID, store.FeedParams{
		FeedURL: "https://example.com/feed.xml", Title: "Example Journal",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	return s, userID, feedID
}

func TestSearchFindsAndRanks(t *testing.T) {
	s, userID, feedID := searchFixture(t)
	ctx := t.Context()

	// The first mentions the term repeatedly and should outrank the passing
	// mention in the second.
	wanted := searchable(t, s, userID, feedID, "gc", "Garbage collection in practice",
		"Garbage collection is the subject here. Garbage collection pauses, garbage "+
			"collection tuning, and the way garbage collection interacts with latency "+
			"budgets are all discussed at length.")
	passing := searchable(t, s, userID, feedID, "aside", "A note on deployments",
		"This is mostly about deployments, though it mentions garbage collection once.")
	searchable(t, s, userID, feedID, "unrelated", "Bread baking",
		"Flour, water, salt, and time. Nothing here concerns software at all.")

	hits, err := s.Search().Query(ctx, userID, store.SearchQuery{Text: "garbage collection"})
	if err != nil {
		t.Fatalf("Query() = %v", err)
	}

	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2:\n%+v", len(hits), hits)
	}
	if hits[0].ArticleID != wanted {
		t.Errorf("top hit is article %d, want %d — the article that is about the term should rank first",
			hits[0].ArticleID, wanted)
	}
	if hits[1].ArticleID != passing {
		t.Errorf("second hit is article %d, want %d", hits[1].ArticleID, passing)
	}
	if hits[0].Rank <= hits[1].Rank {
		t.Errorf("ranks are not ordered: %v then %v", hits[0].Rank, hits[1].Rank)
	}
}

// The snippet is what makes a result list usable, and the highlight has to be the
// only markup in it — it is rendered as HTML.
func TestSearchSnippetHighlightsTheMatch(t *testing.T) {
	s, userID, feedID := searchFixture(t)

	searchable(t, s, userID, feedID, "cephalopod", "On cephalopods",
		"The argonaut is a cephalopod that builds a shell from its own secretions, "+
			"which is unusual among cephalopods and took a long time to explain.")

	hits, err := s.Search().Query(t.Context(), userID, store.SearchQuery{Text: "argonaut"})
	if err != nil {
		t.Fatalf("Query() = %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}

	snippet := hits[0].Snippet
	want := store.HighlightStart + "argonaut" + store.HighlightEnd
	if !strings.Contains(snippet, want) {
		t.Errorf("snippet does not bracket the match with the highlight sentinels: %q", snippet)
	}
	// Sentinels, deliberately not <mark>. The snippet is plain article text, and
	// emitting tags here would make it unsafe to render — see the note on
	// SearchResult.Snippet. The server escapes and then substitutes.
	if strings.Contains(snippet, "<mark>") {
		t.Errorf("snippet contains real markup, which callers would have no way to escape safely: %q", snippet)
	}
}

// websearch_to_tsquery's operators are the reader-facing query language, so they
// have to actually work.
func TestSearchQuerySyntax(t *testing.T) {
	s, userID, feedID := searchFixture(t)
	ctx := t.Context()

	rust := searchable(t, s, userID, feedID, "rust", "Ownership in Rust",
		"Rust uses ownership and borrowing to manage memory without a collector.")
	zig := searchable(t, s, userID, feedID, "zig", "Allocators in Zig",
		"Zig makes allocation explicit, passing an allocator wherever memory is needed.")

	find := func(text string) []store.ArticleID {
		t.Helper()
		hits, err := s.Search().Query(ctx, userID, store.SearchQuery{Text: text})
		if err != nil {
			t.Fatalf("Query(%q) = %v", text, err)
		}
		ids := make([]store.ArticleID, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.ArticleID)
		}
		return ids
	}

	if got := find("ownership borrowing"); len(got) != 1 || got[0] != rust {
		t.Errorf("two bare words = %v, want just the Rust article (implicit AND)", got)
	}
	if got := find(`"explicit"`); len(got) != 1 || got[0] != zig {
		t.Errorf("quoted phrase = %v, want just the Zig article", got)
	}
	if got := find("memory -collector"); len(got) != 1 || got[0] != zig {
		t.Errorf("negation = %v, want just the Zig article", got)
	}
	if got := find("ownership OR allocator"); len(got) != 2 {
		t.Errorf("OR = %v, want both articles", got)
	}
	if got := find("kubernetes"); len(got) != 0 {
		t.Errorf("a term in neither article = %v, want nothing", got)
	}
}

func TestSearchEmptyQueryReturnsNothing(t *testing.T) {
	s, userID, feedID := searchFixture(t)

	searchable(t, s, userID, feedID, "x", "Something", "Some body text about something.")

	for _, text := range []string{"", "   ", "\t\n"} {
		hits, err := s.Search().Query(t.Context(), userID, store.SearchQuery{Text: text})
		if err != nil {
			t.Errorf("Query(%q) = %v, want no error", text, err)
		}
		if len(hits) != 0 {
			t.Errorf("Query(%q) returned %d hits, want none", text, len(hits))
		}
	}
}

func TestSearchRespectsFilters(t *testing.T) {
	s, userID, feedID := searchFixture(t)
	ctx := t.Context()

	read := searchable(t, s, userID, feedID, "read", "Read one", "A shared topic appears here.")
	unread := searchable(t, s, userID, feedID, "unread", "Unread one", "A shared topic appears here too.")

	if _, err := s.SetRead(ctx, userID, read, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}
	if _, err := s.SetStarred(ctx, userID, read, true); err != nil {
		t.Fatalf("SetStarred() = %v", err)
	}

	hits, err := s.Search().Query(ctx, userID, store.SearchQuery{Text: "shared topic", UnreadOnly: true})
	if err != nil {
		t.Fatalf("Query() = %v", err)
	}
	if len(hits) != 1 || hits[0].ArticleID != unread {
		t.Errorf("unread-only search = %+v, want just the unread article", hits)
	}

	hits, err = s.Search().Query(ctx, userID, store.SearchQuery{Text: "shared topic", StarredOnly: true})
	if err != nil {
		t.Fatalf("Query() = %v", err)
	}
	if len(hits) != 1 || hits[0].ArticleID != read {
		t.Errorf("starred-only search = %+v, want just the starred article", hits)
	}
}

// The scoping discipline's specific warning: search must not become the way one reader discovers
// what another has archived.
func TestSearchIsScopedToTheReader(t *testing.T) {
	tr := setupTwoReaders(t)
	ctx := t.Context()

	// A term that appears only in Bob's article.
	if _, err := tr.store.Pool().Exec(ctx, `
		UPDATE article_content SET content_text = 'a distinctive nautilus phrase only bob can read'
		WHERE article_id = $1 AND is_current`, tr.bobOnly); err != nil {
		t.Fatalf("seeding Bob's body: %v", err)
	}

	aliceHits, err := tr.store.Search().Query(ctx, tr.alice, store.SearchQuery{Text: "nautilus"})
	if err != nil {
		t.Fatalf("Query(alice) = %v", err)
	}
	if len(aliceHits) != 0 {
		t.Errorf("Alice's search found %d of Bob's articles: %+v", len(aliceHits), aliceHits)
	}

	bobHits, err := tr.store.Search().Query(ctx, tr.bob, store.SearchQuery{Text: "nautilus"})
	if err != nil {
		t.Fatalf("Query(bob) = %v", err)
	}
	if len(bobHits) != 1 || bobHits[0].ArticleID != tr.bobOnly {
		t.Errorf("Bob's own search = %+v, want his article", bobHits)
	}
}

// M4's acceptance criterion with a number on it: relevant results across the full
// archive in under 200ms at 10,000 articles.
func TestSearchPerformanceAt10kArticles(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://example.com/bulk.xml", Title: "Bulk",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	const total = 10_000
	seedBulk(t, pool, feedID, total)

	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM article_content WHERE is_current`).Scan(&n); err != nil {
		t.Fatalf("counting bodies: %v", err)
	}
	if n < total {
		t.Fatalf("seeded %d bodies, want %d", n, total)
	}

	// ANALYZE, so the planner has statistics. Without it Postgres may pick a
	// sequential scan on a freshly bulk-loaded table and the measurement would be
	// of the planner's ignorance rather than of the query.
	if _, err := pool.Exec(ctx, `ANALYZE articles, article_content, feed_items, feeds, article_state`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	queries := []string{
		"needle",             // rare: one article
		"corpus",             // common: every article
		"\"latency budget\"", // phrase
		"needle OR haystack", // operator
	}

	// Warm once, so the first query does not pay for cold caches on behalf of the
	// measurement.
	if _, err := s.Search().Query(ctx, userID, store.SearchQuery{Text: "warmup"}); err != nil {
		t.Fatalf("warmup Query() = %v", err)
	}

	const budget = 200 * time.Millisecond
	for _, text := range queries {
		var slowest time.Duration
		var hits int

		// Best of three: this runs on whatever the CI box is doing at the time, and
		// one unlucky scheduling hiccup should not fail a latency criterion.
		fastest := time.Hour
		for range 3 {
			start := time.Now()
			results, err := s.Search().Query(ctx, userID, store.SearchQuery{Text: text})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("Query(%q) = %v", text, err)
			}
			hits = len(results)
			if elapsed < fastest {
				fastest = elapsed
			}
			if elapsed > slowest {
				slowest = elapsed
			}
		}

		t.Logf("%-22q %d hits, fastest %v, slowest %v", text, hits, fastest, slowest)
		if fastest > budget {
			t.Errorf("Query(%q) took %v at %d articles, over the %v criterion", text, fastest, total, budget)
		}
	}
}

// seedBulk inserts n articles with bodies and feed references in a handful of
// statements.
//
// Server-side generation rather than n round trips: 10,000 inserts from Go is
// minutes, and this is setup for a latency measurement rather than the thing being
// measured.
func seedBulk(t *testing.T, pool *pgxpool.Pool, feedID store.FeedID, n int) {
	t.Helper()
	ctx := t.Context()

	if _, err := pool.Exec(ctx, `
		INSERT INTO articles (url_canonical, url_original, title, site_name,
		                      published_at, first_seen_at, fetch_status, assets_status)
		SELECT 'https://example.com/bulk/' || i,
		       'https://example.com/bulk/' || i,
		       'Bulk article ' || i,
		       'Bulk Journal',
		       now() - (i || ' minutes')::interval,
		       now() - (i || ' minutes')::interval,
		       'ok', 'none'
		FROM generate_series(1, $1) AS i`, n); err != nil {
		t.Fatalf("bulk inserting articles: %v", err)
	}

	// Bodies with enough variety that ranking has something to do: every article
	// mentions "corpus", one mentions "needle", and a slice of them carry the
	// phrase used to test phrase search.
	if _, err := pool.Exec(ctx, `
		INSERT INTO article_content (article_id, extractor_name, extractor_version,
		                             content_origin, content_html, content_text,
		                             word_count, is_current)
		SELECT a.id, 'trafilatura', '2', 'fetched',
		       '<p>body</p>',
		       'This article belongs to the corpus. '
		         || repeat('Sentences about systems, storage, and scheduling. ', 12)
		         || CASE WHEN a.url_canonical LIKE '%/bulk/7777' THEN 'It contains the needle. ' ELSE '' END
		         || CASE WHEN (a.id % 100) = 0 THEN 'It discusses a latency budget at length. ' ELSE '' END,
		       80, true
		FROM articles a
		WHERE a.url_canonical LIKE 'https://example.com/bulk/%'`); err != nil {
		t.Fatalf("bulk inserting bodies: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO feed_items (feed_id, article_id, guid)
		SELECT $1, a.id, 'bulk-' || a.id
		FROM articles a
		WHERE a.url_canonical LIKE 'https://example.com/bulk/%'`, feedID); err != nil {
		t.Fatalf("bulk inserting feed items: %v", err)
	}

	// A sanity check on the fixture itself: if the rare term is not rare, the
	// measurement below is not measuring what it claims to.
	var rare int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM article_content
		WHERE is_current AND tsv @@ websearch_to_tsquery('english', 'needle')`).Scan(&rare); err != nil {
		t.Fatalf("checking the rare term: %v", err)
	}
	if rare != 1 {
		t.Fatalf("the %q fixture matches %d articles, want exactly 1", "needle", rare)
	}
}
