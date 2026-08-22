package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Asset localization status values, matching the CHECK constraint on
// articles.assets_status.
const (
	AssetsPending = "pending"
	AssetsOK      = "ok"
	AssetsPartial = "partial"
	AssetsNone    = "none"
)

// Asset is a stored image.
//
// Assets are a global pool like articles: an image used by ten articles
// is stored once and shared, which is both correct and the single largest
// storage win in the archive, since images are roughly 80% of its bytes.
type Asset struct {
	SHA256    string
	MediaType string
	ByteSize  int64
	Width     int
	Height    int
	FSPath    string
	SourceURL string
}

// UpsertAsset records a stored image, reporting whether it was new.
//
// Not new means the identical bytes are already in the archive under the same
// content address, so the file did not need writing again. This is the
// deduplication the acceptance criterion is about.
func (s *Store) UpsertAsset(ctx context.Context, a Asset) (bool, error) {
	if a.SHA256 == "" {
		return false, fmt.Errorf("an asset must have a content hash")
	}

	var created bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO assets (sha256, media_type, byte_size, width, height, fs_path, source_url)
		VALUES ($1, $2, $3, NULLIF($4, 0), NULLIF($5, 0), $6, NULLIF($7, ''))
		ON CONFLICT (sha256) DO UPDATE SET
			-- The bytes are identical by definition, so nothing about the file
			-- changes. Only the source URL is filled in when the first sighting
			-- did not record one.
			source_url = COALESCE(assets.source_url, EXCLUDED.source_url)
		RETURNING (xmax = 0)`,
		a.SHA256, a.MediaType, a.ByteSize, a.Width, a.Height, a.FSPath, a.SourceURL,
	).Scan(&created)
	if err != nil {
		return false, fmt.Errorf("recording asset %s: %w", a.SHA256, err)
	}
	return created, nil
}

// LinkAsset records that an article uses an asset.
func (s *Store) LinkAsset(ctx context.Context, articleID ArticleID, sha string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO article_assets (article_id, sha256)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, articleID, sha)
	if err != nil {
		return fmt.Errorf("linking asset %s to article %d: %w", sha, articleID, err)
	}
	return nil
}

// AssetBySourceURL finds an asset already fetched from a URL.
//
// This is a bandwidth optimization, not a correctness requirement: content
// addressing already deduplicates storage after the fetch. Checking first
// means the same image syndicated across ten articles is downloaded once
// instead of ten times, which the origin server notices and appreciates.
//
// The tradeoff is that an image replaced at the same URL will not be
// re-fetched. For an archive that is the right answer — what was published
// with the article is what should be kept, not whatever is there now.
func (s *Store) AssetBySourceURL(ctx context.Context, sourceURL string) (Asset, error) {
	var a Asset
	err := s.pool.QueryRow(ctx, `
		SELECT sha256, media_type, byte_size, COALESCE(width, 0), COALESCE(height, 0),
		       fs_path, COALESCE(source_url, '')
		FROM assets WHERE source_url = $1
		LIMIT 1`, sourceURL,
	).Scan(&a.SHA256, &a.MediaType, &a.ByteSize, &a.Width, &a.Height, &a.FSPath, &a.SourceURL)
	if err != nil {
		return Asset{}, err
	}
	return a, nil
}

// AssetsForArticle returns the images an article uses.
func (s *Store) AssetsForArticle(ctx context.Context, id ArticleID) ([]Asset, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.sha256, a.media_type, a.byte_size, COALESCE(a.width, 0), COALESCE(a.height, 0),
		       a.fs_path, COALESCE(a.source_url, '')
		FROM assets a
		JOIN article_assets aa ON aa.sha256 = a.sha256
		WHERE aa.article_id = $1
		ORDER BY a.fs_path`, id)
	if err != nil {
		return nil, fmt.Errorf("listing assets for article %d: %w", id, err)
	}
	defer rows.Close()

	var out []Asset
	for rows.Next() {
		var a Asset
		if err := rows.Scan(&a.SHA256, &a.MediaType, &a.ByteSize, &a.Width, &a.Height,
			&a.FSPath, &a.SourceURL); err != nil {
			return nil, fmt.Errorf("scanning asset: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAssetsStatus records how localization went for an article.
func (s *Store) SetAssetsStatus(ctx context.Context, id ArticleID, status string) error {
	switch status {
	case AssetsPending, AssetsOK, AssetsPartial, AssetsNone:
	default:
		return fmt.Errorf("invalid assets status %q", status)
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE articles SET assets_status = $2 WHERE id = $1`, id, status); err != nil {
		return fmt.Errorf("setting assets status for article %d: %w", id, err)
	}
	return nil
}

// UpdateContentHTML replaces the body of an article's current content row.
//
// Used after localization, which rewrites image references from origin URLs to
// archive paths. Only the current row is touched: superseded bodies are kept
// as a record of what an earlier extractor produced, and rewriting them would
// destroy that.
func (s *Store) UpdateContentHTML(ctx context.Context, id ArticleID, owner *UserID, html string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE article_content SET content_html = $2
		WHERE article_id = $1 AND is_current
		  AND user_id IS NOT DISTINCT FROM $3`, id, html, owner); err != nil {
		return fmt.Errorf("updating the body of article %d: %w", id, err)
	}
	return nil
}

// PendingAssets returns articles whose images have not been localized yet.
func (s *SystemStore) PendingAssets(ctx context.Context, limit int) ([]ArticleID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id
		FROM articles a
		JOIN article_content c ON c.article_id = a.id AND c.is_current
		WHERE a.assets_status = 'pending'
		ORDER BY a.first_seen_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing articles awaiting asset localization: %w", err)
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

// ArchiveStats summarizes what the archive holds, for `tome archive stats`.
//
// The acceptance criterion asks for storage to be measured against real
// articles and recorded. This is the measurement.
type ArchiveStats struct {
	Articles        int64
	ArticlesFetched int64
	Bodies          int64
	BodyBytes       int64

	Assets        int64
	AssetBytes    int64
	AssetLinks    int64 // article-to-asset references
	AssetsPartial int64
}

// Stats collects archive-wide counts and sizes.
func (s *SystemStore) Stats(ctx context.Context) (ArchiveStats, error) {
	var st ArchiveStats

	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM articles),
			(SELECT count(*) FROM articles WHERE fetch_status = 'ok'),
			(SELECT count(*) FROM article_content WHERE is_current),
			(SELECT COALESCE(sum(length(content_html) + length(content_text)), 0)
			   FROM article_content WHERE is_current),
			(SELECT count(*) FROM assets),
			(SELECT COALESCE(sum(byte_size), 0) FROM assets),
			(SELECT count(*) FROM article_assets),
			(SELECT count(*) FROM articles WHERE assets_status = 'partial')`,
	).Scan(&st.Articles, &st.ArticlesFetched, &st.Bodies, &st.BodyBytes,
		&st.Assets, &st.AssetBytes, &st.AssetLinks, &st.AssetsPartial)
	if err != nil {
		return ArchiveStats{}, fmt.Errorf("collecting archive statistics: %w", err)
	}
	return st, nil
}

// IsNotFound reports whether an error means "no such row", which several
// lookups here treat as an ordinary outcome rather than a failure.
func IsNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
