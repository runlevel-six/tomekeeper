package store

import (
	"context"
	"fmt"
)

// FeedItemParams is one entry in a feed, pointing at an article.
type FeedItemParams struct {
	FeedID    FeedID
	ArticleID ArticleID
	GUID      string
	Title     string
	Summary   string

	// Content is the feed's own body (content:encoded), kept for the last rung
	// of the extraction ladder. It is sometimes the entire article, and it is
	// the only surviving copy when a site goes down between publication and
	// the next poll.
	Content string
}

// InsertFeedItem records that a feed carried a reference to an article,
// reporting whether the reference was new.
//
// The user scope is not decorative here. The INSERT is guarded by an EXISTS
// over feeds for this user, so passing a feed id belonging to someone else
// inserts nothing rather than silently attaching an item to their feed. That
// makes the scope a property of the query rather than of the caller's
// diligence, which is what the scoping discipline asks for.
//
// A GUID already seen in this feed is not an error — it is the normal case on
// every poll after the first, since feeds re-list their recent items.
func (s *Store) InsertFeedItem(ctx context.Context, userID UserID, p FeedItemParams) (bool, error) {
	if p.GUID == "" {
		return false, fmt.Errorf("feed item GUID must not be empty")
	}

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO feed_items (feed_id, article_id, guid, feed_title, feed_summary, feed_content)
		SELECT $1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), NULLIF($7, '')
		WHERE EXISTS (SELECT 1 FROM feeds WHERE id = $1 AND user_id = $6)
		ON CONFLICT (feed_id, guid) DO NOTHING`,
		p.FeedID, p.ArticleID, p.GUID, p.Title, p.Summary, userID, p.Content)
	if err != nil {
		return false, fmt.Errorf("inserting feed item %s: %w", p.GUID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// CountUserArticles returns how many distinct articles a user can see through
// their subscriptions.
//
// This is the shape every user-facing article query must take: it reaches the
// shared articles pool only by joining through that user's feed_items. Reading
// from articles directly would let one user learn of another's saved URLs, so
// the join is the access control.
func (s *Store) CountUserArticles(ctx context.Context, userID UserID) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT i.article_id)
		FROM feed_items i
		JOIN feeds f ON f.id = i.feed_id
		WHERE f.user_id = $1`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting user articles: %w", err)
	}
	return n, nil
}
