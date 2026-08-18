package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/runlevel-six/tomekeeper/internal/urlcanon"
)

// ErrNotSaveable is returned for a URL that cannot be archived.
var ErrNotSaveable = errors.New("not a saveable URL")

// Saved reports what SaveArticle did.
type Saved struct {
	ArticleID ArticleID

	// Canonical is the URL the archive actually keyed on, which may differ from
	// what was pasted — tracking parameters are stripped, hosts are lowercased.
	// Worth showing back to the reader, because "why does my saved link look
	// different" has a good answer and no obvious place to find it.
	Canonical string

	// AlreadySaved is true when this URL was already in the reader's archive.
	AlreadySaved bool

	// HasBody is true when the archive already holds a body for this URL —
	// because a subscribed feed carried it, or because someone else saved it.
	// The page is then readable immediately and no fetch is needed.
	HasBody bool
}

// SaveArticle archives a URL the reader asked for by hand.
//
// This is the manual half of the archive: not something a feed produced, but a
// page someone decided to keep. The plan's §2.2 treats it as just another
// reference to an article, which is what makes it nearly free — a saved page and
// a syndicated one deduplicate to the same row, so saving something already in
// the archive costs nothing and gains its existing body immediately.
//
// No fetch is enqueued here. An article left at 'pending' is swept up by the
// scheduler the worker already runs, so `tome serve` stays a reader and does not
// need to be a job producer. The cost is up to one scheduler interval of latency
// before the body appears, which is the right trade for keeping the web process
// free of the queue.
func (s *Store) SaveArticle(ctx context.Context, userID UserID, rawURL string) (Saved, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return Saved{}, fmt.Errorf("%w: empty", ErrNotSaveable)
	}

	// A pasted URL routinely arrives without a scheme, because that is what a
	// browser's address bar shows. Assuming https is the difference between
	// working and being told off for something that was not a mistake.
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return Saved{}, fmt.Errorf("%w: %q is not a URL", ErrNotSaveable, rawURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return Saved{}, fmt.Errorf("%w: %s is not a web address", ErrNotSaveable, parsed.Scheme)
	}

	canonical, err := urlcanon.Canonicalize(trimmed)
	if err != nil {
		return Saved{}, fmt.Errorf("%w: %w", ErrNotSaveable, err)
	}

	// Title and the rest stay empty: they are what fetching and extraction are
	// for. Guessing them from the URL would put a slug where a headline belongs
	// and leave no way to tell the guess from the real thing later.
	articleID, _, err := s.UpsertArticle(ctx, ArticleParams{
		URLCanonical: canonical,
		URLOriginal:  trimmed,
	})
	if err != nil {
		return Saved{}, fmt.Errorf("recording the saved article: %w", err)
	}

	result := Saved{ArticleID: articleID, Canonical: canonical}

	// saved_at is COALESCEd rather than overwritten, so re-saving keeps the date
	// it was first kept. "When did I save this" should not be reset by a second
	// paste of the same link.
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO article_state (user_id, article_id, saved_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id, article_id) DO UPDATE
		SET saved_at = COALESCE(article_state.saved_at, now())
		RETURNING (xmax <> 0)`,
		userID, articleID).Scan(&result.AlreadySaved); err != nil {
		return Saved{}, fmt.Errorf("saving the article for the reader: %w", err)
	}

	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM article_content WHERE article_id = $1 AND is_current)`,
		articleID).Scan(&result.HasBody); err != nil {
		return Saved{}, fmt.Errorf("checking for an existing body: %w", err)
	}

	return result, nil
}
