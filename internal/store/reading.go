package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// visibleArticles is the access boundary, written once.
//
// See docs/explanation/scoping-and-access-control.md for the reasoning behind
// having one definition rather than a scope repeated per query.
//
// An article is visible to a user when one of their feeds references it, or when
// they starred it themselves. Both routes matter: a subscription is the usual
// one, and `article_state` is how an article the user acted on stays reachable
// even after the feed that introduced it is deleted.
//
// EXISTS rather than a join, so an article carried by three of the user's feeds
// yields one row without a DISTINCT — and DISTINCT over a stream with an excerpt
// column would mean sorting the whole result set to deduplicate rows that were
// never duplicated in the first place.
//
// **Every query that embeds this must pass the user id as $1.** That coupling is
// the price of having one definition instead of five; the scoping discipline is explicit that a
// forgotten scope should be impossible rather than merely unlikely, and one
// predicate that is obviously wrong when misused beats five that are subtly
// right.
const visibleArticles = `(
	EXISTS (
		SELECT 1 FROM feed_items fi
		  JOIN feeds f ON f.id = fi.feed_id
		 WHERE fi.article_id = a.id AND f.user_id = $1
	)
	OR EXISTS (
		SELECT 1 FROM article_state st
		 WHERE st.article_id = a.id AND st.user_id = $1
	)
)`

// ExcerptLength is how much of the body a stream row carries.
//
// Enough for two or three lines under a headline. The stream is the view a reader
// scans, so it must not pull whole articles: at 10,000 rows the difference between
// 320 characters and a full body is the difference between a page and a download.
const ExcerptLength = 320

// StreamOrder is the reading order: newest first, by publication where the feed
// supplied one and by arrival otherwise.
//
// COALESCE rather than published_at alone because a feed that omits dates would
// otherwise sort its entire archive to the bottom, and rather than first_seen_at
// alone because a first poll ingests a decade of history in one second and would
// present it in whatever order the feed happened to list it.
const streamOrder = `COALESCE(a.published_at, a.first_seen_at) DESC, a.id DESC`

// streamOrderReversed is the same order walked upwards, which is what finding
// the article *above* a given one requires.
const streamOrderReversed = `COALESCE(a.published_at, a.first_seen_at) ASC, a.id ASC`

// streamSortKey is the expression both orders sort on, written once because a
// keyset comparison and an ORDER BY that disagree page incorrectly in a way no
// test notices until the boundary row.
const streamSortKey = `COALESCE(a.published_at, a.first_seen_at)`

// StreamQuery selects and pages through a reader's articles.
type StreamQuery struct {
	// UnreadOnly restricts the stream to articles not yet read.
	UnreadOnly bool

	// StarredOnly restricts the stream to starred articles.
	StarredOnly bool

	// SavedOnly restricts the stream to articles the reader saved by hand.
	//
	// Distinct from StarredOnly: starring is a reaction to something a feed
	// brought, saving is a decision to archive something nothing brought. Both
	// set saved_at, so this is a superset of the starred list.
	SavedOnly bool

	// FeedID, when non-zero, restricts the stream to one of the user's feeds.
	FeedID FeedID

	// TagID, when non-zero, restricts the stream to one of the user's tags.
	TagID TagID

	// Category, when Categorized is set, restricts the stream to the articles
	// carried by the user's feeds filed under that category. An empty Category
	// with Categorized set selects the feeds that have none.
	//
	// Two fields rather than one, because "no category" is a real bucket and a
	// bare empty string cannot distinguish it from "do not filter by category" —
	// which is the sort of ambiguity that quietly turns a filter off.
	Category    string
	Categorized bool

	// ReadWithin, when set alongside UnreadOnly, also admits articles read within
	// that window.
	//
	// This exists for one caller: Neighbors. Opening an article marks it read, so
	// a strictly-unread list rearranges itself under the reader the moment they
	// start reading it, and "previous article" would walk off the top of a list
	// that no longer contains anything they have seen. A short window makes the
	// list stable for the length of a reading session without making the unread
	// stream itself lie about what is unread.
	ReadWithin time.Duration

	// Limit caps the page. Zero means DefaultStreamLimit.
	Limit int

	// Before pages backwards through the order above. Both fields come from the
	// last row of the previous page; zero values start at the beginning.
	//
	// Keyset rather than OFFSET, because a reader marking things read while
	// scrolling changes the result set under an offset and silently skips rows.
	BeforeSort time.Time
	BeforeID   ArticleID
}

// DefaultStreamLimit is the page size when a query does not choose one.
const DefaultStreamLimit = 50

// MaxStreamLimit caps what a caller can ask for, so a handcrafted query
// parameter cannot ask for the whole archive in one page.
const MaxStreamLimit = 200

// StreamItem is one row of a stream.
type StreamItem struct {
	ArticleID    ArticleID
	Title        string
	Author       string
	SiteName     string
	URLCanonical string
	PublishedAt  *time.Time
	FirstSeenAt  time.Time
	SortAt       time.Time
	WordCount    int
	Excerpt      string
	Read         bool
	Starred      bool
	Kept         bool
	AssetsStatus string
	FeedTitle    string
	HasBody      bool

	// FetchStatus distinguishes "nothing was extracted from this page" from
	// "nothing has looked at this page yet". A page saved by hand is the second
	// for as long as it takes the worker to reach it, and reporting that as a
	// failure would make the save feature look broken at the moment it worked.
	FetchStatus string
}

// streamFilter is the WHERE clause a StreamQuery describes, together with the
// arguments it references.
//
// Extracted so that Stream and Neighbors cannot disagree about what a list
// contains. They did not, when there was one of them; the moment "the next
// article in this list" became a separate query, two copies of these predicates
// would have been two definitions of the same list, and the bug that produces —
// a Next button that skips an article the list showed — is invisible until it
// happens on somebody's screen.
type streamFilter struct {
	where []string
	args  []any
}

// filter builds the predicates. $1 is always the user id, as visibleArticles
// requires; every other argument is appended and numbered from there, so the
// caller must add its own arguments after these.
func (q StreamQuery) filter(userID UserID) streamFilter {
	f := streamFilter{
		where: []string{visibleArticles},
		args:  []any{userID},
	}

	add := func(clause string, values ...any) {
		for _, v := range values {
			f.args = append(f.args, v)
			clause = strings.Replace(clause, "?", "$"+strconv.Itoa(len(f.args)), 1)
		}
		f.where = append(f.where, clause)
	}

	if q.UnreadOnly {
		// COALESCE, because an article nobody has touched has no state row at all
		// and is unread by definition.
		if q.ReadWithin > 0 {
			add(`(NOT COALESCE(st.read, false)
			      OR st.read_at > now() - make_interval(secs => ?))`, q.ReadWithin.Seconds())
		} else {
			f.where = append(f.where, `NOT COALESCE(st.read, false)`)
		}
	}
	if q.StarredOnly {
		f.where = append(f.where, `COALESCE(st.starred, false)`)
	}
	if q.SavedOnly {
		f.where = append(f.where, `st.saved_at IS NOT NULL`)
	}
	if q.FeedID != 0 {
		add(`EXISTS (SELECT 1 FROM feed_items fi2 JOIN feeds f2 ON f2.id = fi2.feed_id
		             WHERE fi2.article_id = a.id AND f2.id = ? AND f2.user_id = $1)`, q.FeedID)
	}
	if q.TagID != 0 {
		add(`EXISTS (SELECT 1 FROM article_tags at2 JOIN tags t2 ON t2.id = at2.tag_id
		             WHERE at2.article_id = a.id AND t2.id = ? AND t2.user_id = $1)`, q.TagID)
	}
	if q.Categorized {
		// COALESCE on the way in as well as the way out: the column is nullable and
		// an OPML file with a top-level feed stores NULL, so comparing it directly
		// to '' would silently match nothing.
		add(`EXISTS (SELECT 1 FROM feed_items fi4 JOIN feeds f4 ON f4.id = fi4.feed_id
		             WHERE fi4.article_id = a.id AND f4.user_id = $1
		               AND COALESCE(f4.category, '') = ?)`, q.Category)
	}
	return f
}

// Stream returns a page of the user's articles, newest first.
func (s *Store) Stream(ctx context.Context, userID UserID, q StreamQuery) ([]StreamItem, error) {
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = DefaultStreamLimit
	case limit > MaxStreamLimit:
		limit = MaxStreamLimit
	}

	f := q.filter(userID)
	where, args := f.where, f.args

	if !q.BeforeSort.IsZero() {
		args = append(args, q.BeforeSort, q.BeforeID)
		where = append(where, fmt.Sprintf(
			`(`+streamSortKey+`, a.id) < ($%d, $%d)`,
			len(args)-1, len(args)))
	}

	args = append(args, limit)

	query := `
		SELECT a.id, COALESCE(a.title, ''), COALESCE(a.author, ''), COALESCE(a.site_name, ''),
		       a.url_canonical, a.published_at, a.first_seen_at,
		       COALESCE(a.published_at, a.first_seen_at) AS sort_at,
		       a.assets_status, a.fetch_status,
		       COALESCE(c.word_count, 0), left(COALESCE(c.content_text, ''), ` +
		strconv.Itoa(ExcerptLength) + `), (c.id IS NOT NULL),
		       COALESCE(st.read, false), COALESCE(st.starred, false), COALESCE(st.kept, false),
		       COALESCE(feed.title, '')
		FROM articles a
		LEFT JOIN article_content c ON c.article_id = a.id AND c.is_current
		LEFT JOIN article_state st ON st.article_id = a.id AND st.user_id = $1
		-- One feed title per article, chosen deterministically. A syndicated story
		-- reaches the reader through whichever of their feeds saw it first, and
		-- that is the honest attribution to show.
		LEFT JOIN LATERAL (
			SELECT f3.title
			FROM feed_items fi3 JOIN feeds f3 ON f3.id = fi3.feed_id
			WHERE fi3.article_id = a.id AND f3.user_id = $1
			ORDER BY fi3.seen_at, fi3.id
			LIMIT 1
		) feed ON true
		WHERE ` + strings.Join(where, "\n		  AND ") + `
		ORDER BY ` + streamOrder + `
		LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing the stream for user %d: %w", userID, err)
	}
	defer rows.Close()

	var out []StreamItem
	for rows.Next() {
		var it StreamItem
		if err := rows.Scan(
			&it.ArticleID, &it.Title, &it.Author, &it.SiteName,
			&it.URLCanonical, &it.PublishedAt, &it.FirstSeenAt, &it.SortAt,
			&it.AssetsStatus, &it.FetchStatus, &it.WordCount, &it.Excerpt, &it.HasBody,
			&it.Read, &it.Starred, &it.Kept, &it.FeedTitle,
		); err != nil {
			return nil, fmt.Errorf("scanning a stream row: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Neighbors is what sits either side of an article in a list.
//
// Zero means there is nothing there — the reader is at one end of the list.
type Neighbors struct {
	// Newer is the article above this one: the one a reader gets by going back up
	// the list. Named for the direction rather than "previous", because "previous"
	// is ambiguous the moment you ask whether it means earlier in the list or
	// earlier in time.
	Newer ArticleID

	// Older is the article below this one, which is what "next" advances to.
	Older ArticleID
}

// NeighborsIn finds the articles either side of one article within a stream.
//
// The filter comes from a StreamQuery so that "next" means the next article in
// the list the reader was actually looking at — the next thing in Comics, or the
// next unread, rather than the next thing in the archive. Callers pass the same
// query they used to draw that list; see StreamQuery.ReadWithin for the one
// adjustment a reader-facing caller should make.
//
// The current article's position is looked up without the filter applied. That is
// deliberate: an article can stop matching the list it was opened from — the
// obvious case being that opening it marked it read — and its place in the
// ordering is still perfectly well defined.
func (s *Store) NeighborsIn(ctx context.Context, userID UserID, q StreamQuery, id ArticleID) (Neighbors, error) {
	f := q.filter(userID)
	args := append(f.args, id)
	where := strings.Join(f.where, "\n			  AND ")

	// One round trip for both sides. Two queries would be simpler to read and
	// would double the latency of every article page for no gain.
	side := func(comparison, order string) string {
		return `(
			SELECT a.id FROM articles a
			LEFT JOIN article_state st ON st.article_id = a.id AND st.user_id = $1
			WHERE ` + where + `
			  AND (` + streamSortKey + `, a.id) ` + comparison + ` (SELECT sort_at, id FROM cur)
			ORDER BY ` + order + `
			LIMIT 1
		)`
	}

	query := `
		WITH cur AS (
			SELECT ` + streamSortKey + ` AS sort_at, a.id
			FROM articles a
			WHERE a.id = $` + strconv.Itoa(len(args)) + ` AND ` + visibleArticles + `
		)
		SELECT COALESCE(` + side(">", streamOrderReversed) + `, 0),
		       COALESCE(` + side("<", streamOrder) + `, 0)
		FROM cur`

	var n Neighbors
	err := s.pool.QueryRow(ctx, query, args...).Scan(&n.Newer, &n.Older)
	if IsNotFound(err) {
		// The article is not visible to this reader. Its neighbors are nobody's
		// business, and the caller has already decided what to do about the
		// article itself.
		return Neighbors{}, nil
	}
	if err != nil {
		return Neighbors{}, fmt.Errorf("finding the neighbors of article %d for user %d: %w", id, userID, err)
	}
	return n, nil
}

// ArticleView is one article as a reader sees it, with that reader's state.
type ArticleView struct {
	Article Article
	Content Content
	HasBody bool
	Read    bool
	Starred bool
	Kept    bool
	Tags    []Tag

	// ExpiredAt is set when this article's body and images were released by the
	// retention policy. The article is still here; what it said is not.
	ExpiredAt *time.Time
}

// ArticleForUser returns an article the user is allowed to read.
//
// An article outside their visibility is reported as not found rather than
// forbidden. The distinction matters: "forbidden" confirms the article exists,
// which is precisely what the scoping discipline says one user must not be able to infer about
// another's saved URLs.
func (s *Store) ArticleForUser(ctx context.Context, userID UserID, id ArticleID) (ArticleView, error) {
	var (
		v       ArticleView
		content Content
		hasBody bool
	)

	err := s.pool.QueryRow(ctx, `
		SELECT a.id, a.url_canonical, a.url_original,
		       COALESCE(a.title, ''), COALESCE(a.author, ''),
		       COALESCE(a.site_name, ''), COALESCE(a.language, ''),
		       a.published_at, a.first_seen_at,
		       a.fetch_status, COALESCE(a.fetch_error, ''), a.assets_status,
		       COALESCE(a.raw_blob_sha, ''), COALESCE(a.raw_blob_path, ''),
		       (c.id IS NOT NULL),
		       COALESCE(c.extractor_name, ''), COALESCE(c.extractor_version, ''),
		       COALESCE(c.content_origin, ''), COALESCE(c.immutable, false),
		       COALESCE(c.content_html, ''), COALESCE(c.content_text, ''),
		       COALESCE(c.word_count, 0),
		       COALESCE(st.read, false), COALESCE(st.starred, false), COALESCE(st.kept, false),
		       a.content_expired_at
		FROM articles a
		LEFT JOIN article_content c ON c.article_id = a.id AND c.is_current
		LEFT JOIN article_state st ON st.article_id = a.id AND st.user_id = $1
		WHERE a.id = $2 AND `+visibleArticles,
		userID, id,
	).Scan(
		&v.Article.ID, &v.Article.URLCanonical, &v.Article.URLOriginal,
		&v.Article.Title, &v.Article.Author,
		&v.Article.SiteName, &v.Article.Language,
		&v.Article.PublishedAt, &v.Article.FirstSeenAt,
		&v.Article.FetchStatus, &v.Article.FetchError, &v.Article.AssetsStatus,
		&v.Article.RawBlobSHA, &v.Article.RawBlobPath,
		&hasBody,
		&content.ExtractorName, &content.ExtractorVersion,
		&content.ContentOrigin, &content.Immutable,
		&content.HTML, &content.Text, &content.WordCount,
		&v.Read, &v.Starred, &v.Kept, &v.ExpiredAt,
	)
	if err != nil {
		return ArticleView{}, fmt.Errorf("reading article %d for user %d: %w", id, userID, err)
	}

	v.Content = content
	v.HasBody = hasBody

	tags, err := s.TagsForArticle(ctx, userID, id)
	if err != nil {
		return ArticleView{}, err
	}
	v.Tags = tags

	return v, nil
}

// SetRead marks an article read or unread for one user.
//
// Reports whether a row was written. False means the article is not visible to
// this user, which callers should treat exactly as a missing article: allowing a
// state row against an arbitrary id would let one user confirm what another has
// archived, one insert at a time.
func (s *Store) SetRead(ctx context.Context, userID UserID, id ArticleID, read bool) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO article_state (user_id, article_id, read, read_at)
		SELECT $1, a.id, $3, CASE WHEN $3 THEN now() END
		FROM articles a
		WHERE a.id = $2 AND `+visibleArticles+`
		ON CONFLICT (user_id, article_id) DO UPDATE
		SET read = EXCLUDED.read,
		    -- Keep the first time it was read rather than the latest, and clear it
		    -- when it goes back to unread so the column never claims a read that
		    -- was undone.
		    read_at = CASE WHEN EXCLUDED.read
		                   THEN COALESCE(article_state.read_at, now())
		                   ELSE NULL END`,
		userID, id, read)
	if err != nil {
		return false, fmt.Errorf("marking article %d read=%v for user %d: %w", id, read, userID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetStarred stars or unstars an article for one user.
//
// Starring also records saved_at, which is what keeps a starred article reachable
// after the feed that introduced it is gone. Unstarring leaves saved_at alone: the
// reader did save it once, and forgetting that would quietly drop the article out
// of their archive.
func (s *Store) SetStarred(ctx context.Context, userID UserID, id ArticleID, starred bool) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO article_state (user_id, article_id, starred, saved_at)
		SELECT $1, a.id, $3, CASE WHEN $3 THEN now() END
		FROM articles a
		WHERE a.id = $2 AND `+visibleArticles+`
		ON CONFLICT (user_id, article_id) DO UPDATE
		SET starred = EXCLUDED.starred,
		    saved_at = CASE WHEN EXCLUDED.starred
		                    THEN COALESCE(article_state.saved_at, now())
		                    ELSE article_state.saved_at END`,
		userID, id, starred)
	if err != nil {
		return false, fmt.Errorf("marking article %d starred=%v for user %d: %w", id, starred, userID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// UnreadCounts is the per-feed unread tally the feed list shows.
type UnreadCounts struct {
	Total  int64
	ByFeed map[FeedID]int64
}

// UnreadCountsFor returns unread totals for one user.
func (s *Store) UnreadCountsFor(ctx context.Context, userID UserID) (UnreadCounts, error) {
	counts := UnreadCounts{ByFeed: make(map[FeedID]int64)}

	// Counted per feed and then summed in Go rather than with a second query,
	// because an article carried by two of the user's feeds is unread in both and
	// must be one article in the total.
	rows, err := s.pool.Query(ctx, `
		SELECT f.id, count(DISTINCT a.id)
		FROM feeds f
		  JOIN feed_items fi ON fi.feed_id = f.id
		  JOIN articles a ON a.id = fi.article_id
		  LEFT JOIN article_state st ON st.article_id = a.id AND st.user_id = $1
		WHERE f.user_id = $1 AND NOT COALESCE(st.read, false)
		GROUP BY f.id`, userID)
	if err != nil {
		return UnreadCounts{}, fmt.Errorf("counting unread for user %d: %w", userID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id FeedID
			n  int64
		)
		if err := rows.Scan(&id, &n); err != nil {
			return UnreadCounts{}, fmt.Errorf("scanning an unread count: %w", err)
		}
		counts.ByFeed[id] = n
	}
	if err := rows.Err(); err != nil {
		return UnreadCounts{}, err
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM articles a
		LEFT JOIN article_state st ON st.article_id = a.id AND st.user_id = $1
		WHERE `+visibleArticles+` AND NOT COALESCE(st.read, false)`,
		userID).Scan(&counts.Total); err != nil {
		return UnreadCounts{}, fmt.Errorf("counting unread total for user %d: %w", userID, err)
	}

	return counts, nil
}

// NeedsAttention is one entry in the failed-fetch queue.
type NeedsAttention struct {
	ArticleID    ArticleID
	URLCanonical string
	Title        string
	FeedTitle    string
	FetchStatus  string
	FetchError   string
	AssetsStatus string
	FirstSeenAt  time.Time
}

// NeedsAttentionFor lists the user's articles that did not come through cleanly.
//
// Selects on fetch_status, never on assets_status. That is deliberate and was
// learned the hard way: articles whose extraction produced nothing are settled to
// assets_status='none', and before that fix 346 of 1,365 sat at 'pending'
// forever. fetch_status is where the reason lives, and 'skipped' belongs here
// beside 'failed' — a page withheld by robots.txt is a gap in the archive the
// reader should see, not an error to bury.
func (s *Store) NeedsAttentionFor(ctx context.Context, userID UserID, limit int) ([]NeedsAttention, error) {
	if limit <= 0 || limit > MaxStreamLimit {
		limit = DefaultStreamLimit
	}

	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.url_canonical, COALESCE(a.title, ''),
		       COALESCE(feed.title, ''),
		       a.fetch_status, COALESCE(a.fetch_error, ''), a.assets_status,
		       a.first_seen_at
		FROM articles a
		LEFT JOIN LATERAL (
			SELECT f3.title FROM feed_items fi3 JOIN feeds f3 ON f3.id = fi3.feed_id
			WHERE fi3.article_id = a.id AND f3.user_id = $1
			ORDER BY fi3.seen_at, fi3.id LIMIT 1
		) feed ON true
		WHERE `+visibleArticles+`
		  AND (a.fetch_status IN ('failed', 'skipped') OR a.assets_status = 'partial')
		ORDER BY a.first_seen_at DESC, a.id DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing articles needing attention for user %d: %w", userID, err)
	}
	defer rows.Close()

	var out []NeedsAttention
	for rows.Next() {
		var n NeedsAttention
		if err := rows.Scan(&n.ArticleID, &n.URLCanonical, &n.Title, &n.FeedTitle,
			&n.FetchStatus, &n.FetchError, &n.AssetsStatus, &n.FirstSeenAt); err != nil {
			return nil, fmt.Errorf("scanning an attention row: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
