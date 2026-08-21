package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// CategoryID identifies one reader's folder.
type CategoryID int64

// ErrCategoryExists means the reader already has a category by that name.
//
// A distinct error rather than a generic failure because the interface has
// something useful to say about it — the name is taken, by a category they can see —
// and because the unique constraint it comes from is load-bearing: two categories
// with one name would each claim the same feeds on every name-keyed read.
var ErrCategoryExists = errors.New("a category with that name already exists")

// ErrCategoryNameBlank means the name was empty or only spaces.
var ErrCategoryNameBlank = errors.New("a category needs a name")

// CategoryDisposition is what happens to a deleted category's feeds.
//
// There is deliberately no option that touches articles. Nothing in this project
// deletes an article, an article has no category of its own — it is derived through
// feed_items to the feed that carried it — and the destructive intent a reader has
// when deleting a folder is already served by unsubscribing, which exists, asks
// first, and deletes no articles either.
type CategoryDisposition string

const (
	// DispositionUncategorized leaves the feeds subscribed and filed nowhere.
	DispositionUncategorized CategoryDisposition = "uncategorized"

	// DispositionMove refiles them under another category.
	DispositionMove CategoryDisposition = "move"

	// DispositionUnsubscribe drops the subscriptions, which is the existing
	// operation and keeps every article they brought in.
	DispositionUnsubscribe CategoryDisposition = "unsubscribe"
)

// CreateCategory adds an empty category.
//
// Empty is the point: it is the thing free text could not express, and the reason a
// reader could not make a folder and then move feeds into it.
func (s *Store) CreateCategory(ctx context.Context, userID UserID, name string) (CategoryID, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, ErrCategoryNameBlank
	}

	var id CategoryID
	err := s.pool.QueryRow(ctx,
		`INSERT INTO categories (user_id, name) VALUES ($1, $2) RETURNING id`,
		userID, name).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrCategoryExists
		}
		return 0, fmt.Errorf("creating category %q for user %d: %w", name, userID, err)
	}
	return id, nil
}

// RenameCategory changes a category's name, leaving its feeds where they are.
//
// This is the operation the old design could not perform without rewriting every
// feed in the folder — and the one that used to break Fever clients, because the
// group id was derived from the name. The id does not change here, so a client's
// cached folder membership survives.
func (s *Store) RenameCategory(ctx context.Context, userID UserID, id CategoryID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrCategoryNameBlank
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE categories SET name = $3 WHERE id = $2 AND user_id = $1`, userID, id, name)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrCategoryExists
		}
		return fmt.Errorf("renaming category %d for user %d: %w", id, userID, err)
	}
	if tag.RowsAffected() == 0 {
		// Not found, never forbidden — the same rule articles and feeds follow. A
		// distinct refusal would confirm that somebody else's category exists.
		return pgx.ErrNoRows
	}
	return nil
}

// DeleteCategoryResult reports what a deletion did, for the page that asked.
type DeleteCategoryResult struct {
	// Feeds is how many subscriptions the disposition applied to.
	Feeds int64

	// Unsubscribed is how many were dropped, which is only ever non-zero for
	// DispositionUnsubscribe.
	Unsubscribed int64
}

// DeleteCategory removes a category and disposes of its feeds.
//
// One transaction, because a category deleted while its feeds kept pointing at it —
// or feeds refiled under a category that then failed to delete — is a state no page
// can describe. **No article is deleted, moved, or altered by any branch of this.**
func (s *Store) DeleteCategory(
	ctx context.Context, userID UserID, id CategoryID,
	how CategoryDisposition, into CategoryID,
) (DeleteCategoryResult, error) {
	var result DeleteCategoryResult

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("starting the category deletion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Confirmed to be theirs before anything is touched, so a hand-crafted request
	// cannot reach another reader's folder — and so the row count below means what it
	// says.
	var owned bool
	if err := tx.QueryRow(ctx,
		`SELECT true FROM categories WHERE id = $2 AND user_id = $1`, userID, id,
	).Scan(&owned); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return result, pgx.ErrNoRows
		}
		return result, fmt.Errorf("looking up category %d: %w", id, err)
	}

	switch how {
	case DispositionMove:
		// The destination has to be theirs too, and cannot be the category being
		// deleted — which would refile the feeds into a row about to disappear and
		// leave them uncategorized by accident rather than by choice.
		if into == id {
			return result, fmt.Errorf("cannot move a category's feeds into itself")
		}
		var target bool
		if err := tx.QueryRow(ctx,
			`SELECT true FROM categories WHERE id = $2 AND user_id = $1`, userID, into,
		).Scan(&target); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return result, pgx.ErrNoRows
			}
			return result, fmt.Errorf("looking up the destination category: %w", err)
		}

		tag, err := tx.Exec(ctx,
			`UPDATE feeds SET category_id = $3 WHERE user_id = $1 AND category_id = $2`,
			userID, id, into)
		if err != nil {
			return result, fmt.Errorf("refiling the feeds of category %d: %w", id, err)
		}
		result.Feeds = tag.RowsAffected()

	case DispositionUnsubscribe:
		// Deliberately the same shape as unsubscribing one feed: the subscription and
		// its feed_items go, and no article does. Articles left unreachable by this
		// are the same residue a single unsubscribe leaves, and `tome prune` is where
		// that is answered — not here.
		var ids []FeedID
		rows, err := tx.Query(ctx,
			`SELECT id FROM feeds WHERE user_id = $1 AND category_id = $2`, userID, id)
		if err != nil {
			return result, fmt.Errorf("listing the feeds of category %d: %w", id, err)
		}
		for rows.Next() {
			var fid FeedID
			if err := rows.Scan(&fid); err != nil {
				rows.Close()
				return result, fmt.Errorf("scanning a feed id: %w", err)
			}
			ids = append(ids, fid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return result, fmt.Errorf("listing the feeds of category %d: %w", id, err)
		}

		for _, fid := range ids {
			if _, err := tx.Exec(ctx,
				`DELETE FROM feed_items WHERE feed_id = $1`, fid); err != nil {
				return result, fmt.Errorf("clearing the items of feed %d: %w", fid, err)
			}
			if _, err := tx.Exec(ctx,
				`DELETE FROM feeds WHERE id = $1 AND user_id = $2`, fid, userID); err != nil {
				return result, fmt.Errorf("unsubscribing feed %d: %w", fid, err)
			}
		}
		result.Feeds = int64(len(ids))
		result.Unsubscribed = int64(len(ids))

	case DispositionUncategorized:
		// Nothing to do to the feeds: the foreign key is ON DELETE SET NULL, so
		// deleting the row files them nowhere. Counted first so the page can say how
		// many it affected.
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM feeds WHERE user_id = $1 AND category_id = $2`,
			userID, id).Scan(&result.Feeds); err != nil {
			return result, fmt.Errorf("counting the feeds of category %d: %w", id, err)
		}

	default:
		return result, fmt.Errorf("unknown disposition %q", how)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM categories WHERE id = $2 AND user_id = $1`, userID, id); err != nil {
		return result, fmt.Errorf("deleting category %d: %w", id, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("committing the category deletion: %w", err)
	}
	return result, nil
}

// CategoryByName finds one of a reader's categories.
func (s *Store) CategoryByName(ctx context.Context, userID UserID, name string) (CategoryID, error) {
	var id CategoryID
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM categories WHERE user_id = $1 AND name = $2`,
		userID, strings.TrimSpace(name)).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// SetFeedCategory files one feed under a category, or under none.
//
// A nil category means no category, which is the absence of a row rather than a row
// named for the absence — see the 00013 migration for why that distinction is worth
// keeping.
func (s *Store) SetFeedCategory(ctx context.Context, userID UserID, feedID FeedID, category *CategoryID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE feeds SET category_id = $3 WHERE id = $2 AND user_id = $1`,
		userID, feedID, category)
	if err != nil {
		return fmt.Errorf("filing feed %d: %w", feedID, err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// isUniqueViolation reports whether an error is Postgres refusing a duplicate.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
