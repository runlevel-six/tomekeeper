package store

import (
	"context"
	"fmt"
	"time"
)

// Expirable is an article whose stored body and images can be released.
type Expirable struct {
	ArticleID ArticleID
	Title     string
	URL       string
}

// Expired reports what an expiry actually freed.
//
// The blob paths are returned rather than deleted here, because the store owns
// the database and not the filesystem. The caller deletes the files, and does so
// only after the transaction has committed: a file deleted for a transaction
// that then rolls back is unrecoverable, whereas a row that survives a file
// deletion is merely wrong in a way that can be fixed.
type Expired struct {
	ArticleID  ArticleID
	BodyBytes  int64
	AssetBytes int64
	AssetPaths []string
	RawPath    string
}

// ExpirableArticles finds articles that every reader has finished with.
//
// The hard part is that article_content and assets are a *shared* pool (§2.8).
// One reader finishing an article says nothing about whether it can be deleted:
// another may have it unread, starred, or saved. So this asks a global question —
// is there anybody left with a claim — and an article is only expirable when the
// answer is no.
//
// The three conditions, in the order they matter:
//
//   - Somebody has finished with it. Without this an article nobody ever opened
//     would expire the moment it aged past the cutoff, which is the opposite of
//     what a reading backlog is for.
//   - Nobody holds a claim. Unread, starred, kept, or saved by any user blocks
//     it, as does having been read too recently.
//   - No subscriber has ignored it. A user subscribed to a feed that carried this
//     article but who has no state row at all has never seen it, which is unread
//     by definition and not something to quietly delete.
//
// Every ambiguity resolves toward keeping. A read row with no read_at — possible
// for anything marked read before that column was populated — blocks expiry
// rather than being assumed old, because the cost of guessing wrong is an
// article that cannot be got back.
func (s *Store) ExpirableArticles(ctx context.Context, cutoff time.Time, limit int) ([]Expirable, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.pool.Query(ctx, `
		SELECT a.id, COALESCE(a.title, ''), a.url_canonical
		FROM articles a
		WHERE a.content_expired_at IS NULL
		  AND EXISTS (SELECT 1 FROM article_content c WHERE c.article_id = a.id)

		  -- Somebody finished with it, long enough ago.
		  AND EXISTS (
		    SELECT 1 FROM article_state st
		    WHERE st.article_id = a.id
		      AND st.read AND NOT st.starred AND NOT st.kept
		      AND st.saved_at IS NULL
		      AND st.read_at IS NOT NULL AND st.read_at < $1)

		  -- And nobody still has a claim on it.
		  AND NOT EXISTS (
		    SELECT 1 FROM article_state st
		    WHERE st.article_id = a.id
		      AND (NOT st.read
		           OR st.starred
		           OR st.kept
		           OR st.saved_at IS NOT NULL
		           OR st.read_at IS NULL
		           OR st.read_at >= $1))

		  -- And no subscriber has simply not got to it yet.
		  AND NOT EXISTS (
		    SELECT 1 FROM feed_items fi
		    JOIN feeds f ON f.id = fi.feed_id
		    WHERE fi.article_id = a.id
		      AND NOT EXISTS (
		        SELECT 1 FROM article_state st2
		        WHERE st2.article_id = a.id AND st2.user_id = f.user_id))

		ORDER BY a.id
		LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("finding expirable articles: %w", err)
	}
	defer rows.Close()

	var out []Expirable
	for rows.Next() {
		var e Expirable
		if err := rows.Scan(&e.ArticleID, &e.Title, &e.URL); err != nil {
			return nil, fmt.Errorf("scanning an expirable article: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading expirable articles: %w", err)
	}
	return out, nil
}

// ExpireArticle releases one article's body and images.
//
// The article row survives, along with everyone's read and starred state. What
// goes is article_content, the raw stored page, and any asset this was the last
// article referencing.
//
// "Last article referencing" is the whole reason this is not a DELETE cascade:
// assets are content-addressed and shared, so an image used by three articles
// must survive the expiry of two of them. Deleting it with the first would
// silently break the other two, and content addressing means nothing would ever
// notice.
func (s *Store) ExpireArticle(ctx context.Context, id ArticleID) (Expired, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Expired{}, fmt.Errorf("starting the expiry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result := Expired{ArticleID: id}

	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(length(content_html) + length(content_text)), 0)
		FROM article_content WHERE article_id = $1`, id).Scan(&result.BodyBytes); err != nil {
		return Expired{}, fmt.Errorf("measuring the body of article %d: %w", id, err)
	}

	// Assets this article is the last to reference. Evaluated before the join
	// rows are deleted, because afterwards there is nothing left to ask.
	assetRows, err := tx.Query(ctx, `
		SELECT s.sha256, s.fs_path, s.byte_size
		FROM assets s
		WHERE s.sha256 IN (SELECT sha256 FROM article_assets WHERE article_id = $1)
		  AND NOT EXISTS (
		    SELECT 1 FROM article_assets other
		    WHERE other.sha256 = s.sha256 AND other.article_id <> $1)`, id)
	if err != nil {
		return Expired{}, fmt.Errorf("finding the orphaned assets of article %d: %w", id, err)
	}

	var orphans []string
	for assetRows.Next() {
		var (
			sha, path string
			size      int64
		)
		if err := assetRows.Scan(&sha, &path, &size); err != nil {
			assetRows.Close()
			return Expired{}, fmt.Errorf("scanning an orphaned asset: %w", err)
		}
		orphans = append(orphans, sha)
		result.AssetPaths = append(result.AssetPaths, path)
		result.AssetBytes += size
	}
	assetRows.Close()
	if err := assetRows.Err(); err != nil {
		return Expired{}, fmt.Errorf("reading the orphaned assets: %w", err)
	}

	// Read before write. RETURNING reports the *new* row, so asking for
	// raw_blob_path in the same statement that clears it would return the empty
	// string every time — and the stored page it named would sit on disk forever,
	// with nothing left in the database pointing at it.
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(raw_blob_path, '') FROM articles WHERE id = $1`, id).Scan(&result.RawPath); err != nil {
		return Expired{}, fmt.Errorf("reading the stored page path of article %d: %w", id, err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE articles
		SET content_expired_at = now(), assets_status = 'none',
		    raw_blob_path = NULL, raw_blob_sha = NULL
		WHERE id = $1`, id); err != nil {
		return Expired{}, fmt.Errorf("marking article %d expired: %w", id, err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM article_content WHERE article_id = $1`, id); err != nil {
		return Expired{}, fmt.Errorf("deleting the body of article %d: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM article_assets WHERE article_id = $1`, id); err != nil {
		return Expired{}, fmt.Errorf("unlinking the assets of article %d: %w", id, err)
	}
	if len(orphans) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM assets WHERE sha256 = ANY($1)`, orphans); err != nil {
			return Expired{}, fmt.Errorf("deleting the orphaned assets of article %d: %w", id, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Expired{}, fmt.Errorf("committing the expiry of article %d: %w", id, err)
	}
	return result, nil
}

// SetKept marks an article as never to be expired, or lifts that mark.
//
// Separate from starring on purpose: starring also protects an article, but this
// exists for the ones worth holding onto without having liked them.
//
// Matches the shape of SetRead and SetStarred so the HTTP toggle can be written
// once for all three.
func (s *Store) SetKept(ctx context.Context, userID UserID, id ArticleID, kept bool) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO article_state (user_id, article_id, kept)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, article_id) DO UPDATE SET kept = EXCLUDED.kept`,
		userID, id, kept)
	if err != nil {
		return false, fmt.Errorf("setting kept on article %d: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}
