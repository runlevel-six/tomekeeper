package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Fetch status values, matching the CHECK constraint on articles.fetch_status.
const (
	FetchPending = "pending"
	FetchOK      = "ok"
	FetchFailed  = "failed"
	FetchSkipped = "skipped"
)

// Content origins, recorded on each body. Provenance belongs to the
// body, not to the shared article row.
const (
	OriginFetched  = "fetched"
	OriginFeedBody = "feed_body"
)

// FetchedPage is what one successful fetch produced.
//
// A struct rather than three positional arguments because the third one is a boolean,
// and `RecordFetchSuccess(ctx, id, sha, path, true)` at a call site says nothing about
// what is true.
type FetchedPage struct {
	// SHA is the SHA-256 of the stored bytes, and Path is where they went.
	SHA  string
	Path string

	// BrowserRendered records that these bytes came out of a headless browser rather
	// than off the wire — see migration 00007 for why this is stored rather than
	// inferred from the domain rules in force when somebody asks.
	BrowserRendered bool
}

// RecordFetchSuccess marks an article fetched and records where the raw page
// was stored.
//
// Articles are a global pool, so this takes no UserID: the fetched page is the
// same page whoever's subscription led to it.
func (s *Store) RecordFetchSuccess(ctx context.Context, id ArticleID, p FetchedPage) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE articles SET
			raw_blob_sha     = $2,
			raw_blob_path    = $3,
			raw_fetched_at   = now(),
			fetch_status     = 'ok',
			fetch_error      = NULL,
			browser_rendered = $4
		WHERE id = $1`, id, p.SHA, p.Path, p.BrowserRendered)
	if err != nil {
		return fmt.Errorf("recording fetch of article %d: %w", id, err)
	}
	return nil
}

// RecordFetchWaiting notes why an article has not been fetched yet, without saying it
// failed.
//
// One caller: a render that found no browser. That is not the article's failure and not
// the site's — it is an operator's deployment scaled to zero, or a pod that died — and
// recording it as `failed` would be wrong twice over. It would blame the site for
// infrastructure, and because a recorded failure is never retried, it would make a
// transient condition permanent.
//
// So the status stays `pending`, which keeps the article eligible for the scheduler to
// try again, and the reason goes in `fetch_error` anyway. That pairing — pending *with* a
// reason — is what the failed-fetch queue now selects on, and it is the difference between
// an article that is waiting for a browser and one the worker simply has not reached.
// Before this existed the two were indistinguishable, and the interface reported both as
// "queued".
//
// Deliberately does not touch assets_status. RecordFetchFailure settles it because every
// path into that function is the pipeline stopping for good; this one is the pipeline
// pausing, and an article that later renders successfully needs its images localized like
// any other.
func (s *Store) RecordFetchWaiting(ctx context.Context, id ArticleID, reason string) error {
	if reason == "" {
		return fmt.Errorf("a waiting reason must not be empty")
	}

	// Only while the article is still pending. A render queued against an article that
	// has since been fetched by another route must not overwrite a settled state with a
	// note about waiting.
	_, err := s.pool.Exec(ctx, `
		UPDATE articles SET fetch_error = $2
		WHERE id = $1 AND fetch_status = 'pending'`, id, reason)
	if err != nil {
		return fmt.Errorf("recording that article %d is waiting: %w", id, err)
	}
	return nil
}

// RecordExtractAttempt notes that extraction ran, at which version, and what it found in
// the page.
//
// Two facts from one write, because they are produced together and are both about the
// attempt rather than about its result:
//
//   - **The version.** A body records the version that produced it, so a *failure* had
//     nowhere to record one — which meant `tome reextract` could not tell a failure from
//     an older extractor apart from a failure under the current one, and so excluded all
//     of them. See migration 00009 for what that cost.
//   - **The page measurement**, which is what the failed-fetch queue uses to say whether a
//     page is a JavaScript shell wanting a browser or a structural problem wanting a CSS
//     selector. Those have opposite remedies and were indistinguishable in the interface.
//
// Called on every attempt, successful or not. On success the body carries the version too
// and this column is redundant — but "the last attempt was at version N" staying true
// regardless of outcome is easier to reason about than a column that means something
// different depending on whether there is a body next to it.
func (s *Store) RecordExtractAttempt(
	ctx context.Context, id ArticleID, version string, pageVisibleChars int,
) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE articles
		SET extract_attempt_version = $2,
		    page_visible_chars      = $3
		WHERE id = $1`, id, version, pageVisibleChars)
	if err != nil {
		return fmt.Errorf("recording the extraction attempt for article %d: %w", id, err)
	}
	return nil
}

// RecordFetchFailure marks an article as failed to fetch, with the reason.
//
// The reason is kept because the failed-fetch queue is what makes the
// extraction tail routine maintenance rather than a mystery: a list of
// articles with "HTTP 403" against them is a list of domains that need a rule
// or an apology.
//
// It also settles assets_status, because every caller of this is a point where
// the pipeline stops: a fetch that was refused, skipped by robots.txt, or an
// extraction that produced nothing all mean no localization job will ever run.
// Leaving the column at 'pending' made it a terminal state wearing a transient
// label — and one the asset scheduler could never clear, since PendingAssets
// inner-joins the current content row that these articles do not have. Measured
// against a real feed list, 346 of 1,365 articles sat 'pending' forever.
//
// 'none' rather than a new value: the asset policy defines it as "the body had no qualifying
// images", and an article with no body vacuously has none. That keeps the
// vocabulary and needs no migration.
func (s *Store) RecordFetchFailure(ctx context.Context, id ArticleID, status, reason string) error {
	if status != FetchFailed && status != FetchSkipped {
		return fmt.Errorf("invalid fetch status %q", status)
	}

	// The CASE is deliberately narrow. Only a row that is still 'pending' *and*
	// has no current body is unreachable; anything already 'ok' or 'partial' has
	// localized images that a later failed re-fetch must not erase, and a
	// 'pending' row that does have a body is one the scheduler can still see and
	// will process normally.
	_, err := s.pool.Exec(ctx, `
		UPDATE articles
		SET fetch_status = $2,
		    fetch_error  = $3,
		    assets_status = CASE
		        WHEN assets_status = $4
		         AND NOT EXISTS (
		               SELECT 1 FROM article_content c
		               WHERE c.article_id = articles.id AND c.is_current)
		        THEN $5
		        ELSE assets_status
		    END
		WHERE id = $1`,
		id, status, reason, AssetsPending, AssetsNone)
	if err != nil {
		return fmt.Errorf("recording fetch failure for article %d: %w", id, err)
	}
	return nil
}

// GetArticle returns one article by id.
func (s *Store) GetArticle(ctx context.Context, id ArticleID) (Article, error) {
	var a Article
	err := s.pool.QueryRow(ctx, `
		SELECT id, url_canonical, url_original,
		       COALESCE(title, ''), COALESCE(author, ''),
		       COALESCE(site_name, ''), COALESCE(language, ''),
		       published_at, first_seen_at, fetch_status, COALESCE(fetch_error, ''),
		       assets_status, COALESCE(raw_blob_sha, ''), COALESCE(raw_blob_path, ''),
		       browser_rendered
		FROM articles WHERE id = $1`, id,
	).Scan(&a.ID, &a.URLCanonical, &a.URLOriginal, &a.Title, &a.Author,
		&a.SiteName, &a.Language, &a.PublishedAt, &a.FirstSeenAt, &a.FetchStatus,
		&a.FetchError, &a.AssetsStatus, &a.RawBlobSHA, &a.RawBlobPath, &a.BrowserRendered)
	if err != nil {
		return Article{}, fmt.Errorf("looking up article %d: %w", id, err)
	}
	return a, nil
}

// FeedBodyFor returns the fullest body any feed carried for an article.
//
// The longest wins: the same story syndicated through several feeds is
// commonly truncated in one and complete in another, and the complete one is
// the only version worth keeping when the page itself cannot be fetched.
func (s *Store) FeedBodyFor(ctx context.Context, id ArticleID) (string, error) {
	var body string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(feed_content, feed_summary, '')
		FROM feed_items
		WHERE article_id = $1
		ORDER BY length(COALESCE(feed_content, feed_summary, '')) DESC
		LIMIT 1`, id).Scan(&body)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// A manually saved article has no feed behind it. Not an error.
		return "", nil
	case err != nil:
		return "", fmt.Errorf("looking up the feed body for article %d: %w", id, err)
	}
	return body, nil
}

// ContentParams is one extracted body, ready to be stored.
type ContentParams struct {
	ArticleID        ArticleID
	ExtractorName    string
	ExtractorVersion string
	ContentOrigin    string
	Immutable        bool
	HTML             string
	Text             string
	WordCount        int
	FSPath           string
}

// InsertContent stores a new body and makes it current.
//
// Extraction is versioned, so an earlier body is never destroyed — it is
// demoted. That is what lets a bad extractor release be diagnosed after the
// fact, and it costs a row.
//
// An immutable current body is never demoted. An imported Wallabag
// entry may be the only surviving copy of a dead URL, so a later successful
// fetch of the same article is stored alongside it, not over it, and promoting
// it is a deliberate human act.
func (s *Store) InsertContent(ctx context.Context, p ContentParams) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentIsImmutable bool
	err = tx.QueryRow(ctx, `
		SELECT immutable FROM article_content
		WHERE article_id = $1 AND is_current`, p.ArticleID).Scan(&currentIsImmutable)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("checking the current body of article %d: %w", p.ArticleID, err)
	}

	makeCurrent := !currentIsImmutable
	if makeCurrent {
		// The partial unique index permits one current row per article, so the
		// old one has to be demoted before the new one is inserted.
		if _, err := tx.Exec(ctx, `
			UPDATE article_content SET is_current = false
			WHERE article_id = $1 AND is_current`, p.ArticleID); err != nil {
			return false, fmt.Errorf("demoting the previous body: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO article_content (
			article_id, extractor_name, extractor_version, content_origin,
			immutable, content_html, content_text, word_count, is_current, fs_path
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''))`,
		p.ArticleID, p.ExtractorName, p.ExtractorVersion, p.ContentOrigin,
		p.Immutable, p.HTML, p.Text, p.WordCount, makeCurrent, p.FSPath,
	); err != nil {
		return false, fmt.Errorf("inserting the body of article %d: %w", p.ArticleID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return makeCurrent, nil
}

// Content is a stored body.
type Content struct {
	ArticleID        ArticleID
	ExtractorName    string
	ExtractorVersion string
	ContentOrigin    string
	Immutable        bool
	HTML             string
	Text             string
	WordCount        int
	ExtractedAt      time.Time
}

// CurrentContent returns the body currently shown for an article.
func (s *Store) CurrentContent(ctx context.Context, id ArticleID) (Content, error) {
	var c Content
	err := s.pool.QueryRow(ctx, `
		SELECT article_id, extractor_name, extractor_version, content_origin,
		       immutable, content_html, content_text, COALESCE(word_count, 0), extracted_at
		FROM article_content
		WHERE article_id = $1 AND is_current`, id,
	).Scan(&c.ArticleID, &c.ExtractorName, &c.ExtractorVersion, &c.ContentOrigin,
		&c.Immutable, &c.HTML, &c.Text, &c.WordCount, &c.ExtractedAt)
	if err != nil {
		return Content{}, fmt.Errorf("looking up the body of article %d: %w", id, err)
	}
	return c, nil
}

// UpdateArticleMetadata fills gaps in an article's metadata from what an
// extractor recovered from the page.
//
// Only gaps: whatever was already there came from a feed or an earlier
// extraction that has been seen and is not worth churning. The one exception
// is that an extractor is trusted for language, which feeds routinely get
// wrong by declaring the site's language rather than the article's.
func (s *Store) UpdateArticleMetadata(ctx context.Context, id ArticleID, p ArticleParams) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE articles SET
			title        = COALESCE(title, NULLIF($2, '')),
			author       = COALESCE(author, NULLIF($3, '')),
			site_name    = COALESCE(site_name, NULLIF($4, '')),
			language     = COALESCE(NULLIF($5, ''), language),
			published_at = COALESCE(published_at, $6)
		WHERE id = $1`,
		id, p.Title, p.Author, p.SiteName, p.Language, p.PublishedAt)
	if err != nil {
		return fmt.Errorf("updating metadata for article %d: %w", id, err)
	}
	return nil
}

// ReextractCandidate is an article whose body should be regenerated.
type ReextractCandidate struct {
	ArticleID   ArticleID
	RawBlobPath string
}

// ReextractCandidates returns articles whose stored page was last extracted by an
// extractor version other than the given one, starting after afterID.
//
// **Both outcomes count.** An article with a body from an older extractor is a candidate,
// and so is an article whose last extraction produced *nothing* under an older extractor.
// The second half was missing until 2026-08-21, and its absence was the most expensive
// bug in this file: reprocessing joined the current content row, so an article with no
// body could not be selected, so every extraction improvement silently skipped exactly
// the articles improvements are written for. Measured when it was found: 343 articles with
// a stored page and no body, 280 of them webcomics that the image rung added three
// versions earlier would have archived. The pages were on disk the whole time.
//
// This lives on SystemStore because reprocessing the archive is a maintenance
// operation over the shared article pool, not a user's view of it.
//
// Immutable bodies are excluded in the query rather than skipped in the
// caller. That is the point of the acceptance criterion: an imported article
// must be *provably* untouched by a bulk reprocess, and a WHERE clause is a
// proof, while a conditional in a loop is a promise.
//
// Pagination is by id cursor rather than OFFSET. Queueing a job does not
// change the row's extractor version — the worker does that later — so an
// offset-free repeat of the same query would return the same rows forever and
// only ever reprocess the first batch.
//
// An empty domain matches every article. A non-empty one restricts to that host
// and its subdomains, matching how a domain rule applies: a rule written for
// example.com governs blog.example.com, so a reprocess scoped to example.com has
// to cover the same set or the flag would quietly do less than the rule it exists
// to apply.
func (s *SystemStore) ReextractCandidates(
	ctx context.Context, beforeVersion, domain string, afterID ArticleID, limit int,
) ([]ReextractCandidate, error) {
	// Compared against the article's host rather than with a LIKE over the whole
	// URL. `LIKE '%example.com%'` would also match notexample.com, and — worse —
	// evil.com/?ref=example.com, which is a path an attacker controls.
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))

	// The host is derived once in a subquery rather than twice in the WHERE, so the
	// two comparisons cannot drift apart. A plain subquery rather than a CTE, so the
	// planner is free to push the predicates down instead of materializing every
	// candidate first.
	// A LEFT JOIN, so an article with no current body reaches the WHERE clause at all.
	// The two branches below are the two outcomes an extraction can have, and each needs
	// its own version to compare against:
	//
	//   - A body carries the version that produced it, and `NOT c.immutable` applies only
	//     here because immutability is a property of a body. An imported article stays
	//     provably untouched by a bulk reprocess, which is the acceptance criterion this
	//     clause exists for.
	//   - A failure has no body, so it carries the version on the article instead.
	//     **IS DISTINCT FROM, not `<>`**: the column is NULL for every article extracted
	//     before it existed, and `NULL <> '5'` is NULL rather than true — a plain
	//     inequality would silently exclude every article this branch was added to reach,
	//     which is the same shape of bug it is fixing.
	//
	// An empty raw_blob_path is excluded rather than merely NULL-checked. There is nothing
	// to extract from either way, and queueing those produced a job whose only outcome was
	// to record "no stored page" against an article that already said so.
	rows, err := s.pool.Query(ctx, `
		SELECT id, raw_blob_path FROM (
			SELECT a.id AS id,
			       COALESCE(a.raw_blob_path, '') AS raw_blob_path,
			       -- scheme://host[:port]/path -> host
			       split_part(split_part(split_part(a.url_canonical, '://', 2), '/', 1), ':', 1) AS host
			FROM articles a
			LEFT JOIN article_content c ON c.article_id = a.id AND c.is_current
			WHERE COALESCE(a.raw_blob_path, '') <> ''
			  AND a.id > $2
			  AND (
			        (c.id IS NOT NULL AND NOT c.immutable AND c.extractor_version <> $1)
			     OR (c.id IS NULL AND a.extract_attempt_version IS DISTINCT FROM $1)
			  )
		) candidates
		WHERE $4 = '' OR host = $4 OR host LIKE '%.' || $4
		ORDER BY id
		LIMIT $3`, beforeVersion, afterID, limit, host)
	if err != nil {
		return nil, fmt.Errorf("listing reextract candidates: %w", err)
	}
	defer rows.Close()

	var out []ReextractCandidate
	for rows.Next() {
		var c ReextractCandidate
		if err := rows.Scan(&c.ArticleID, &c.RawBlobPath); err != nil {
			return nil, fmt.Errorf("scanning reextract candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PendingFetch returns articles that have not been fetched yet.
//
// The scheduler uses this to drain a backlog of articles ingested before there
// was a fetcher, and to pick up
// anything whose fetch job was lost. It spans users because the article pool
// is shared; nothing user-specific is returned.
func (s *SystemStore) PendingFetch(ctx context.Context, limit int) ([]ArticleID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM articles
		WHERE fetch_status = 'pending'
		ORDER BY first_seen_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing articles awaiting fetch: %w", err)
	}
	defer rows.Close()

	var ids []ArticleID
	for rows.Next() {
		var id ArticleID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning article id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ExtractionStats counts bodies by extractor, for the acceptance criterion
// and for the feed health view.
type ExtractionStats struct {
	Extractor string
	Articles  int64
	MedianLen int64
}

// ExtractionStats reports how many current bodies each extractor produced.
func (s *SystemStore) ExtractionStats(ctx context.Context) ([]ExtractionStats, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT extractor_name, count(*),
		       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY length(content_text)), 0)::bigint
		FROM article_content
		WHERE is_current
		GROUP BY extractor_name
		ORDER BY count(*) DESC`)
	if err != nil {
		return nil, fmt.Errorf("collecting extraction stats: %w", err)
	}
	defer rows.Close()

	var out []ExtractionStats
	for rows.Next() {
		var st ExtractionStats
		if err := rows.Scan(&st.Extractor, &st.Articles, &st.MedianLen); err != nil {
			return nil, fmt.Errorf("scanning extraction stats: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
