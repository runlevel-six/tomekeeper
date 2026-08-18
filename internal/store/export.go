package store

import (
	"context"
	"fmt"
	"time"
)

// ExportRow is one article as an export sees it: the article, the reader's
// relationship to it, and the body currently shown.
//
// Deliberately flat and deliberately not ArticleView. The reader's view is built
// for a page and fetches one article at a time; an export walks an entire archive
// and cannot afford a round trip per article, so this is what one row of a keyset
// scan yields.
type ExportRow struct {
	ArticleID ArticleID

	URLCanonical string
	URLOriginal  string
	Title        string
	Author       string
	SiteName     string
	Language     string
	PublishedAt  *time.Time
	FirstSeenAt  time.Time

	// RawBlobPath is where the original page is stored, relative to the blob root.
	// Carried so an export can say where the bytes are without inlining a decade of
	// compressed HTML into a JSON document.
	RawBlobPath string

	// SavedAt, Read and Starred are this reader's state. SavedAt is null for an
	// article that arrived from a feed and was never saved by hand.
	SavedAt *time.Time
	Read    bool
	Starred bool
	Kept    bool

	// The body currently shown, empty when the article has none — a failed fetch,
	// or a body released by the retention policy.
	ContentHTML      string
	ContentText      string
	WordCount        int
	ExtractorName    string
	ExtractorVersion string
	ContentOrigin    string
	Immutable        bool

	// SourceName and SourceID are the import bookkeeping, when this article came
	// from another system. Carried so that a round trip preserves provenance: an
	// archive restored from an export still knows which of its articles came from
	// a Wallabag library, and a later re-import of that library still recognizes
	// them rather than adding them twice.
	SourceName string
	SourceID   string
}

// ExportArticles returns a page of the reader's archive, ordered by article id.
//
// Ordered by id and paged on it, rather than by date: an export is a complete walk
// and needs an order that is total, stable, and cheap. Dates are neither total —
// two articles can share one — nor stable, since a re-extraction can change what a
// row says about itself.
func (s *Store) ExportArticles(ctx context.Context, userID UserID, after ArticleID, limit int,
) ([]ExportRow, error) {
	if limit <= 0 {
		limit = 500
	}

	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.url_canonical, a.url_original,
		       COALESCE(a.title, ''), COALESCE(a.author, ''),
		       COALESCE(a.site_name, ''), COALESCE(a.language, ''),
		       a.published_at, a.first_seen_at, COALESCE(a.raw_blob_path, ''),
		       st.saved_at,
		       COALESCE(st.read, false), COALESCE(st.starred, false), COALESCE(st.kept, false),
		       COALESCE(c.content_html, ''), COALESCE(c.content_text, ''),
		       COALESCE(c.word_count, 0), COALESCE(c.extractor_name, ''),
		       COALESCE(c.extractor_version, ''), COALESCE(c.content_origin, ''),
		       COALESCE(c.immutable, false),
		       COALESCE(ir.source_name, ''), COALESCE(ir.source_id, '')
		FROM articles a
		LEFT JOIN article_content c ON c.article_id = a.id AND c.is_current
		LEFT JOIN article_state st ON st.article_id = a.id AND st.user_id = $1
		LEFT JOIN import_records ir ON ir.article_id = a.id AND ir.user_id = $1
		WHERE a.id > $2 AND `+visibleArticles+`
		ORDER BY a.id
		LIMIT $3`, userID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("exporting articles for user %d: %w", userID, err)
	}
	defer rows.Close()

	var out []ExportRow
	for rows.Next() {
		var r ExportRow
		if err := rows.Scan(
			&r.ArticleID, &r.URLCanonical, &r.URLOriginal,
			&r.Title, &r.Author, &r.SiteName, &r.Language,
			&r.PublishedAt, &r.FirstSeenAt, &r.RawBlobPath,
			&r.SavedAt, &r.Read, &r.Starred, &r.Kept,
			&r.ContentHTML, &r.ContentText, &r.WordCount,
			&r.ExtractorName, &r.ExtractorVersion, &r.ContentOrigin, &r.Immutable,
			&r.SourceName, &r.SourceID,
		); err != nil {
			return nil, fmt.Errorf("scanning an export row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExportAsset is one localized image, as an export records it.
type ExportAsset struct {
	// Path is where the bytes are in the blob tree. An export names them rather
	// than carrying them: a decade of images is not something to base64 into a
	// JSON document, and the archive on disk already holds them.
	Path      string
	SourceURL string
	SHA256    string
	MediaType string
	ByteSize  int64
	Width     int
	Height    int
}

// AssetsForArticles returns the localized images of a page of articles, keyed by
// article.
//
// One query for the page rather than one per article. An export of ten thousand
// articles is the case this exists for, and per-article queries are what make a
// walk of that size take minutes instead of seconds.
func (s *Store) AssetsForArticles(ctx context.Context, ids []ArticleID) (map[ArticleID][]ExportAsset, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT aa.article_id, s.fs_path, COALESCE(s.source_url, ''), s.sha256,
		       s.media_type, s.byte_size, COALESCE(s.width, 0), COALESCE(s.height, 0)
		FROM article_assets aa
		JOIN assets s ON s.sha256 = aa.sha256
		WHERE aa.article_id = ANY($1)
		ORDER BY aa.article_id, s.fs_path`, ids)
	if err != nil {
		return nil, fmt.Errorf("listing assets for export: %w", err)
	}
	defer rows.Close()

	byArticle := make(map[ArticleID][]ExportAsset, len(ids))
	for rows.Next() {
		var (
			id    ArticleID
			asset ExportAsset
		)
		if err := rows.Scan(&id, &asset.Path, &asset.SourceURL, &asset.SHA256,
			&asset.MediaType, &asset.ByteSize, &asset.Width, &asset.Height); err != nil {
			return nil, fmt.Errorf("scanning an export asset: %w", err)
		}
		byArticle[id] = append(byArticle[id], asset)
	}
	return byArticle, rows.Err()
}

// TagsForArticles returns the reader's tags on a page of articles, keyed by
// article. One query for the page, for the same reason as the assets.
func (s *Store) TagsForArticles(ctx context.Context, userID UserID, ids []ArticleID,
) (map[ArticleID][]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT at.article_id, t.name
		FROM article_tags at
		JOIN tags t ON t.id = at.tag_id
		WHERE t.user_id = $1 AND at.article_id = ANY($2)
		ORDER BY at.article_id, t.name`, userID, ids)
	if err != nil {
		return nil, fmt.Errorf("listing tags for export: %w", err)
	}
	defer rows.Close()

	byArticle := make(map[ArticleID][]string, len(ids))
	for rows.Next() {
		var (
			id   ArticleID
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scanning an export tag: %w", err)
		}
		byArticle[id] = append(byArticle[id], name)
	}
	return byArticle, rows.Err()
}

// HighlightsForArticles returns the reader's highlights on a page of articles,
// keyed by article.
func (s *Store) HighlightsForArticles(ctx context.Context, userID UserID, ids []ArticleID,
) (map[ArticleID][]ImportHighlight, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT article_id, quote, COALESCE(note, ''), created_at
		FROM highlights
		WHERE user_id = $1 AND article_id = ANY($2)
		ORDER BY article_id, created_at, id`, userID, ids)
	if err != nil {
		return nil, fmt.Errorf("listing highlights for export: %w", err)
	}
	defer rows.Close()

	byArticle := make(map[ArticleID][]ImportHighlight, len(ids))
	for rows.Next() {
		var (
			id        ArticleID
			h         ImportHighlight
			createdAt time.Time
		)
		if err := rows.Scan(&id, &h.Quote, &h.Note, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning an export highlight: %w", err)
		}
		h.CreatedAt = &createdAt
		byArticle[id] = append(byArticle[id], h)
	}
	return byArticle, rows.Err()
}

// CountExportable is how many articles an export will contain.
func (s *Store) CountExportable(ctx context.Context, userID UserID) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM articles a WHERE `+visibleArticles, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting exportable articles for user %d: %w", userID, err)
	}
	return n, nil
}
