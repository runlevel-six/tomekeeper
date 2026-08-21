package store

import (
	"context"
	"fmt"
)

// Prunable is one article nothing points at any more, with what releasing it would
// free.
type Prunable struct {
	Expirable

	// Bytes is the stored body and its exclusively-held images, which is what
	// releasing this article would actually recover. Reported before anything
	// happens, because "prune 812 articles" is not a decision anybody can make and
	// "recover 140 MB" is.
	Bytes int64
}

// PrunableArticles lists articles no subscription references and nobody has acted on.
//
// This is the residue unsubscribing leaves, and it accumulates silently. Retention
// cannot reach it: ExpirableArticles requires `read AND read_at < cutoff`, so an
// article that arrived, was never opened, and then lost its feed is never expirable
// at any setting. Unsubscribing deliberately deletes no articles — re-subscribing
// relinks them by canonical URL — so nothing has ever collected them.
//
// Three conditions, and each is a decision:
//
//   - **No feed references it.** Not "the feed is disabled" or "the feed failed" —
//     no row in feed_items at all, which is what unsubscribing leaves behind.
//   - **Nobody has acted on it.** No article_state row from any reader: not read,
//     starred, kept, saved, or annotated. An article somebody starred is reachable
//     through their starred list forever, feed or no feed, so it is not residue.
//     This is deliberately across all readers rather than scoped to one, which is
//     why it lives beside the other system-wide operations: an article one reader
//     abandoned may be another's.
//   - **Its body is not immutable.** An imported body may be the only surviving copy
//     of a page that no longer exists, so releasing it is not "it can be fetched
//     again", it is losing the article. Retention states the same rule in the same
//     way for the same reason.
//
// It releases bodies rather than deleting rows, which was the maintainer's call and
// is the conservative one: the archive keeps knowing the article existed, the
// operation reuses ExpireArticle exactly, and nothing has to reckon with the two
// foreign keys that would block a delete.
func (s *Store) PrunableArticles(ctx context.Context, limit int) ([]Prunable, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.pool.Query(ctx, `
		SELECT a.id, COALESCE(a.title, ''), a.url_canonical,
		       COALESCE(sum(length(c.content_html) + length(c.content_text)), 0)
		         + COALESCE((
		             -- Images this article is the last to hold. Counted the same way
		             -- ExpireArticle decides what to unlink, so the number reported
		             -- is the number recovered rather than an estimate of it.
		             SELECT sum(s2.byte_size) FROM assets s2
		             WHERE s2.sha256 IN (SELECT sha256 FROM article_assets WHERE article_id = a.id)
		               AND NOT EXISTS (
		                 SELECT 1 FROM article_assets other
		                 WHERE other.sha256 = s2.sha256 AND other.article_id <> a.id)
		           ), 0) AS bytes
		FROM articles a
		JOIN article_content c ON c.article_id = a.id
		WHERE a.content_expired_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM feed_items fi WHERE fi.article_id = a.id)
		  AND NOT EXISTS (SELECT 1 FROM article_state st WHERE st.article_id = a.id)
		  AND NOT EXISTS (
		        SELECT 1 FROM article_content ic
		        WHERE ic.article_id = a.id AND ic.immutable)
		GROUP BY a.id, a.title, a.url_canonical
		-- Biggest first, so a run with a limit frees the most it can.
		ORDER BY bytes DESC, a.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing prunable articles: %w", err)
	}
	defer rows.Close()

	var out []Prunable
	for rows.Next() {
		var p Prunable
		if err := rows.Scan(&p.ArticleID, &p.Title, &p.URL, &p.Bytes); err != nil {
			return nil, fmt.Errorf("scanning a prunable article: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
