package store

import (
	"context"
	"fmt"
	"time"
)

// Article is a row of the articles table.
//
// Articles are a global pool shared by every user (§2.8): two people
// subscribed to the same site get one archived copy and one set of images.
// Methods here therefore take no UserID, and that is deliberate rather than an
// oversight — a user's *relationship* to an article lives in feed_items,
// article_state, and import_records, all of which are scoped.
type Article struct {
	ID           ArticleID
	URLCanonical string
	URLOriginal  string
	Title        string
	Author       string
	SiteName     string
	Language     string
	PublishedAt  *time.Time
	FirstSeenAt  time.Time
	FetchStatus  string
}

// ArticleParams is the set of fields a reference can contribute about an
// article it points to.
type ArticleParams struct {
	URLCanonical string
	URLOriginal  string
	Title        string
	Author       string
	SiteName     string
	Language     string
	PublishedAt  *time.Time
}

// UpsertArticle inserts an article or returns the existing one, reporting
// whether it was created.
//
// This is where deduplication happens: the same story syndicated through three
// feeds canonicalizes to one URL and collapses to one row, which is what makes
// the article the root entity rather than the feed item.
//
// On conflict, existing values win and only missing ones are filled in.
// Whichever reference saw the article first is treated as authoritative
// because it is the one whose metadata has been reviewed, and because a feed
// that supplies an empty title should not be able to erase a good one.
func (s *Store) UpsertArticle(ctx context.Context, p ArticleParams) (ArticleID, bool, error) {
	if p.URLCanonical == "" {
		return 0, false, fmt.Errorf("canonical URL must not be empty")
	}
	if p.URLOriginal == "" {
		p.URLOriginal = p.URLCanonical
	}

	var (
		id      ArticleID
		created bool
	)
	err := s.pool.QueryRow(ctx, `
		INSERT INTO articles (
			url_canonical, url_original, title, author, site_name, language, published_at
		)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), $7)
		ON CONFLICT (url_canonical) DO UPDATE SET
			title        = COALESCE(articles.title, EXCLUDED.title),
			author       = COALESCE(articles.author, EXCLUDED.author),
			site_name    = COALESCE(articles.site_name, EXCLUDED.site_name),
			language     = COALESCE(articles.language, EXCLUDED.language),
			published_at = COALESCE(articles.published_at, EXCLUDED.published_at)
		RETURNING id, (xmax = 0)`,
		p.URLCanonical, p.URLOriginal, p.Title, p.Author, p.SiteName, p.Language, p.PublishedAt,
	).Scan(&id, &created)
	if err != nil {
		return 0, false, fmt.Errorf("upserting article %s: %w", p.URLCanonical, err)
	}
	return id, created, nil
}

// GetArticleByURL returns the article with the given canonical URL.
func (s *Store) GetArticleByURL(ctx context.Context, canonical string) (Article, error) {
	var a Article
	err := s.pool.QueryRow(ctx, `
		SELECT id, url_canonical, url_original,
		       COALESCE(title, ''), COALESCE(author, ''),
		       COALESCE(site_name, ''), COALESCE(language, ''),
		       published_at, first_seen_at, fetch_status
		FROM articles
		WHERE url_canonical = $1`, canonical,
	).Scan(&a.ID, &a.URLCanonical, &a.URLOriginal, &a.Title, &a.Author,
		&a.SiteName, &a.Language, &a.PublishedAt, &a.FirstSeenAt, &a.FetchStatus)
	if err != nil {
		return Article{}, fmt.Errorf("looking up article %s: %w", canonical, err)
	}
	return a, nil
}

// CountArticles returns the total size of the shared article pool. It backs
// the import summary and the `--dry-run` report.
func (s *Store) CountArticles(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM articles`).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting articles: %w", err)
	}
	return n, nil
}
