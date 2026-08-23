package store

import (
	"context"
	"fmt"
	"strings"
)

// SearchIndex is the seam that lets a different search engine be dropped in
// without touching the handlers that search.
//
// Index, Query, Delete. Postgres satisfies them
// with `article_content.tsv`, a generated column the database maintains itself —
// which is why Index and Delete do nothing here. That is worth saying out loud,
// because two methods with empty bodies otherwise read as an unfinished job rather
// than as the point: an engine that keeps its own copy of the text needs to be told
// when an article changes, and one that derives it from the row does not. Call sites
// must invoke them regardless, so that swapping engines is a constructor change and
// nothing else.
//
// Declared beside its Postgres implementation rather than in the consumer, because
// Query has to be scoped by the same access predicate as every other read
// (visibleArticles) and duplicating that predicate into another package to satisfy
// a convention would trade a real safety property for a stylistic one.
type SearchIndex interface {
	// Index makes one article's current body searchable.
	Index(ctx context.Context, id ArticleID) error

	// Query searches the articles one user is allowed to read.
	Query(ctx context.Context, userID UserID, q SearchQuery) ([]SearchResult, error)

	// Delete removes an article from the index.
	Delete(ctx context.Context, id ArticleID) error
}

// SearchQuery is a search request.
type SearchQuery struct {
	// Text is the reader's query, in the syntax websearch_to_tsquery accepts:
	// bare words, "quoted phrases", OR, and a leading - to exclude.
	Text string

	// UnreadOnly and StarredOnly narrow the search the same way the stream does.
	UnreadOnly  bool
	StarredOnly bool

	// Limit caps results. Zero means DefaultStreamLimit.
	Limit int
}

// SearchResult is one hit.
type SearchResult struct {
	ArticleID    ArticleID
	Title        string
	SiteName     string
	URLCanonical string
	FeedTitle    string
	Rank         float32

	// Snippet is the matching passage, with matched terms bracketed by
	// HighlightStart and HighlightEnd.
	//
	// **Plain text, not HTML, and it must be escaped before rendering.** It is built
	// by ts_headline from content_text, which is the article's *text* — so an
	// article about markup legitimately contains things like <script>, and treating
	// this as safe HTML would inject them. The sentinels exist so a caller can
	// escape the whole string and then substitute real <mark> tags; anything that
	// was in the article stays inert.
	Snippet string

	Read    bool
	Starred bool
}

// searchConfig is the text search configuration, hardcoded to english.
//
// A per-row configuration cannot be used with a generated column: to_tsvector is
// immutable, but the text::regconfig cast needed to read a per-row config is only
// stable, so Postgres rejects the expression. The migration path when non-English
// content actually exists: drop the generated column, keep a plain tsvector
// maintained by the application on write, and reindex. Do not pre-empt it.
const searchConfig = "english"

// HighlightStart and HighlightEnd bracket a matched term in a Snippet.
//
// Not <mark> directly, because the snippet is plain article text that may contain
// angle brackets of its own. These are substituted for real tags *after* escaping,
// so the only markup that survives is the highlight. If an article happens to
// contain one of these literally the result is a stray highlight — visibly odd, and
// harmless, which is the correct way for this to fail.
const (
	HighlightStart = "[[hl]]"
	HighlightEnd   = "[[/hl]]"
)

// PostgresSearch implements SearchIndex over the generated tsv column.
//
// A distinct type rather than methods on Store, because `Index`, `Query`, and
// `Delete` are names that say nothing on a data-access type — and a method called
// `Store.Delete(ctx, id)` that quietly does nothing is a trap someone will fall
// into looking for a way to remove an article.
type PostgresSearch struct{ store *Store }

// Search returns the Postgres-backed search index.
func (s *Store) Search() *PostgresSearch { return &PostgresSearch{store: s} }

// Query implements SearchIndex.
//
// Joins through the same visibility predicate as every other read. The scoping discipline is explicit
// about why: searching `article_content` directly would let one reader discover the
// existence of another's saved URLs by typing a guess into the search box, which is
// a leak no amount of care in the handler would fix.
func (p *PostgresSearch) Query(ctx context.Context, userID UserID, q SearchQuery) ([]SearchResult, error) {
	// An empty query matches nothing, and websearch_to_tsquery would agree — but
	// returning early means a reader who submits the form without typing does not
	// pay for a scan to be told so.
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}

	sql, args := p.statement(userID, q)

	rows, err := p.store.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("searching for user %d: %w", userID, err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ArticleID, &r.Title, &r.SiteName, &r.URLCanonical,
			&r.FeedTitle, &r.Rank, &r.Snippet, &r.Read, &r.Starred); err != nil {
			return nil, fmt.Errorf("scanning a search result: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Index implements SearchIndex, and deliberately does nothing.
//
// `article_content.tsv` is `GENERATED ALWAYS AS (to_tsvector(...)) STORED`, so
// Postgres has already indexed the row by the time this could be called. The method
// exists so that call sites are written as though the index needed maintaining,
// which is what makes a different engine a drop-in rather than a rewrite.
func (p *PostgresSearch) Index(_ context.Context, _ ArticleID) error { return nil }

// Delete implements SearchIndex, and deliberately does nothing.
//
// The generated column goes with the row: deleting an article cascades to
// `article_content` and takes its index entry with it. See Index.
func (p *PostgresSearch) Delete(_ context.Context, _ ArticleID) error { return nil }

var _ SearchIndex = (*PostgresSearch)(nil)

// statement builds the search SQL and its arguments.
//
// Extracted so that ExplainQuery can ask the database about the *same* statement
// the reader's search runs. A test that explains a hand-copied approximation
// proves nothing about the query that ships.
func (p *PostgresSearch) statement(userID UserID, q SearchQuery) (string, []any) {
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = DefaultStreamLimit
	case limit > MaxStreamLimit:
		limit = MaxStreamLimit
	}

	// A hit is a match in the body **or** in the title, and the two are gathered by
	// separate branches rather than by an OR in one WHERE clause. That is not a
	// stylistic choice: the body's index is on `article_content` and the title's is on
	// `articles`, and PostgreSQL cannot combine index scans on *different relations*
	// into one bitmap. Written as `c.tsv @@ tsq OR a.title_tsv @@ tsq` the planner
	// abandons both indexes and sequentially scans the archive — measured at 10,000
	// articles, a cost estimate of 93,645 against a plan that had been using the GIN.
	// The performance test explains the real statement for exactly this reason.
	//
	// Each branch is scoped as tightly as the final query. The body branch carries
	// preferredBody, because `article_content` holds other readers' forks and matching
	// their text would leak the existence of a body that is not yours. Visibility is
	// applied once, at the end, over the union.
	where := []string{visibleArticles}
	if q.UnreadOnly {
		where = append(where, `NOT COALESCE(st.read, false)`)
	}
	if q.StarredOnly {
		where = append(where, `COALESCE(st.starred, false)`)
	}

	// MATERIALIZED, so the query text is parsed into a tsquery once rather than per
	// branch — and so the two branches provably search for the same thing.
	sql := `
		WITH q AS MATERIALIZED (SELECT websearch_to_tsquery($3, $2) AS tsq),
		body_hits AS (
			SELECT c.article_id, ts_rank_cd(c.tsv, q.tsq, 32) AS body_rank
			FROM q, article_content c
			JOIN articles a ON a.id = c.article_id
			WHERE c.is_current AND c.tsv @@ q.tsq AND ` + preferredBody + `
		),
		title_hits AS (
			SELECT a.id AS article_id FROM q, articles a WHERE a.title_tsv @@ q.tsq
		),
		hits AS (
			SELECT article_id, max(body_rank) AS body_rank, bool_or(is_title) AS title_hit
			FROM (
				SELECT article_id, body_rank, false AS is_title FROM body_hits
				UNION ALL
				SELECT article_id, 0::float4, true FROM title_hits
			-- Aliased "matches" rather than "both": BOTH is a reserved word in
			-- PostgreSQL, and the syntax error it produces points at the alias rather
			-- than at the reason. (No backticks in here either — they end the Go raw
			-- string this query lives in, which this file has now paid for twice.)
			) matches
			GROUP BY article_id
		)
		SELECT a.id, COALESCE(a.title, ''), COALESCE(a.site_name, ''), a.url_canonical,
		       COALESCE(feed.title, ''),
		       -- A title match outranks every body-only match, and normalization is
		       -- what makes that true rather than usually true. Bare ts_rank_cd is
		       -- unbounded and grows with how often a term occurs — a body repeating a
		       -- word forty times scored above 1 and beat the article named after it,
		       -- which is how this was found. Flag 32 is rank/(rank+1), so a body
		       -- scores in [0,1), the flat +1 for a title always wins, and the order
		       -- among body matches is unchanged because the transform is monotonic.
		       --
		       -- If the words are in the title, that is the article somebody meant.
		       COALESCE(h.body_rank, 0) + CASE WHEN h.title_hit THEN 1 ELSE 0 END AS rank,
		       -- Empty for an article matched on its title alone, or one with no body
		       -- at all: an article that failed extraction is exactly the one somebody
		       -- looks for by the title they remember, so it has to be findable, and
		       -- there is nothing to quote from. The title is rendered above the
		       -- snippet either way.
		       COALESCE(ts_headline($3, c.content_text, q.tsq,
		           'StartSel=[[hl]], StopSel=[[/hl]], MaxWords=40, MinWords=20, MaxFragments=2, FragmentDelimiter=" … "'), ''),
		       COALESCE(st.read, false), COALESCE(st.starred, false)
		FROM hits h
		CROSS JOIN q
		JOIN articles a ON a.id = h.article_id
		` + ownedBody + `
		LEFT JOIN article_state st ON st.article_id = a.id AND st.user_id = $1
		LEFT JOIN LATERAL (
			SELECT f3.title FROM feed_items fi3 JOIN feeds f3 ON f3.id = fi3.feed_id
			WHERE fi3.article_id = a.id AND f3.user_id = $1
			ORDER BY fi3.seen_at, fi3.id LIMIT 1
		) feed ON true
		WHERE ` + strings.Join(where, "\n\t\t  AND ") + `
		ORDER BY rank DESC, a.id DESC
		LIMIT $4`

	return sql, []any{userID, q.Text, searchConfig, limit}
}

// ExplainQuery returns PostgreSQL's plan for a search.
//
// Exposed because "is search still using the index" is a question worth being able
// to answer directly — in a test, and on a real archive when search has become
// slow. Timing a query tells you it is slow; the plan tells you why.
func (p *PostgresSearch) ExplainQuery(ctx context.Context, userID UserID, q SearchQuery) (string, error) {
	sql, args := p.statement(userID, q)

	rows, err := p.store.pool.Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		return "", fmt.Errorf("explaining the search query: %w", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", fmt.Errorf("scanning the query plan: %w", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	return plan.String(), rows.Err()
}
