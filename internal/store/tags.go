package store

import (
	"context"
	"fmt"
	"strings"
)

// TagID identifies a tag. A distinct type for the same reason UserID is one: a
// bare int64 passed to the wrong parameter compiles.
type TagID int64

// Tag is one of a user's labels.
type Tag struct {
	ID    TagID
	Name  string
	Count int64
}

// EnsureTag returns the id of a user's tag, creating it if necessary.
//
// Names are trimmed and compared case-insensitively on the way in, so "Rust" and
// "rust" are one tag rather than two that look identical in a list. The first
// spelling wins, because it is the one the reader chose.
func (s *Store) EnsureTag(ctx context.Context, userID UserID, name string) (TagID, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("tag name must not be empty")
	}

	var id TagID

	// Case-insensitive lookup first, then insert. The unique index is on the exact
	// name, so this cannot be a single upsert without either a functional index or
	// accepting two casings of one tag.
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM tags WHERE user_id = $1 AND lower(name) = lower($2)`,
		userID, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !IsNotFound(err) {
		return 0, fmt.Errorf("looking up tag %q for user %d: %w", name, userID, err)
	}

	if err := s.pool.QueryRow(ctx, `
		INSERT INTO tags (user_id, name) VALUES ($1, $2)
		ON CONFLICT (user_id, name) DO UPDATE SET name = tags.name
		RETURNING id`, userID, name).Scan(&id); err != nil {
		return 0, fmt.Errorf("creating tag %q for user %d: %w", name, userID, err)
	}
	return id, nil
}

// ListTags returns a user's tags with how many of their articles carry each.
//
// The count joins through the article's visibility, not just article_tags, so a
// tag cannot report articles the reader can no longer see.
func (s *Store) ListTags(ctx context.Context, userID UserID) ([]Tag, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.name, count(a.id)
		FROM tags t
		LEFT JOIN article_tags at ON at.tag_id = t.id
		LEFT JOIN articles a ON a.id = at.article_id AND `+visibleArticles+`
		WHERE t.user_id = $1
		GROUP BY t.id, t.name
		ORDER BY t.name`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing tags for user %d: %w", userID, err)
	}
	defer rows.Close()

	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Count); err != nil {
			return nil, fmt.Errorf("scanning a tag: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TagsForArticle returns the user's tags on one article.
func (s *Store) TagsForArticle(ctx context.Context, userID UserID, id ArticleID) ([]Tag, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.name
		FROM article_tags at
		  JOIN tags t ON t.id = at.tag_id
		WHERE at.article_id = $2 AND t.user_id = $1
		ORDER BY t.name`, userID, id)
	if err != nil {
		return nil, fmt.Errorf("listing tags on article %d for user %d: %w", id, userID, err)
	}
	defer rows.Close()

	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, fmt.Errorf("scanning an article tag: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TagArticle attaches one of the user's tags to an article they can see.
//
// Reports whether anything was written. False means the article is not visible or
// the tag is not theirs — `article_tags` has no user column, so the scoping has to
// come from both sides, and getting it from only one would let a reader label
// somebody else's article or borrow somebody else's tag.
func (s *Store) TagArticle(ctx context.Context, userID UserID, id ArticleID, tagID TagID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO article_tags (article_id, tag_id)
		SELECT a.id, t.id
		FROM articles a, tags t
		WHERE a.id = $2 AND t.id = $3 AND t.user_id = $1 AND `+visibleArticles+`
		ON CONFLICT DO NOTHING`,
		userID, id, tagID)
	if err != nil {
		return false, fmt.Errorf("tagging article %d for user %d: %w", id, userID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// UntagArticle removes one of the user's tags from an article.
func (s *Store) UntagArticle(ctx context.Context, userID UserID, id ArticleID, tagID TagID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM article_tags at
		USING tags t
		WHERE at.tag_id = t.id
		  AND at.article_id = $2
		  AND t.id = $3
		  AND t.user_id = $1`,
		userID, id, tagID)
	if err != nil {
		return false, fmt.Errorf("untagging article %d for user %d: %w", id, userID, err)
	}
	return tag.RowsAffected() > 0, nil
}
