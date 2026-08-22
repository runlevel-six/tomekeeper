package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ContentID identifies one stored body of an article.
type ContentID int64

// StoredBody is one of an article's bodies, as the reader chooses between them.
//
// An article can hold several: the page as this archive extracted it, the copy a
// library was imported with, and an older extraction kept when a better one
// replaced it. Only one is shown, and which one is a decision rather than an
// accident — which is what this type exists to make possible.
type StoredBody struct {
	ID        ContentID
	ArticleID ArticleID

	ExtractorName    string
	ExtractorVersion string
	ContentOrigin    string
	Immutable        bool

	// Owner is nil for the household's extraction and set for this reader's own.
	//
	// The chooser lists both, so the page has to be able to say which is which:
	// "what everyone gets" and "the one you picked" are different offers, and a
	// list that showed them identically would make promoting look like it had done
	// nothing.
	Owner *UserID

	WordCount   int
	ExtractedAt time.Time
	Current     bool

	// Excerpt is the opening of the body, which is how a reader tells two of them
	// apart without opening both.
	Excerpt string
}

// bodyExcerptLength is how much of each body the chooser shows.
//
// Enough to see whether a body starts with the article or with a cookie banner,
// which is the usual difference between the one worth showing and the one that is
// not.
const bodyExcerptLength = 240

// BodiesForArticle lists the bodies of an article this reader may choose between,
// newest first.
//
// Their own and the household's — never another reader's. The comment this
// replaces said bodies are a property of the article rather than of a reader,
// which was true while there was one reader and is the assumption tenancy
// removes: another reader's extraction is theirs, and listing it here would offer
// it and leak that they have one.
//
// Callers must still establish that this reader may see the article at all;
// choosing between bodies is a narrower question than being allowed to look.
func (s *Store) BodiesForArticle(ctx context.Context, userID UserID, id ArticleID) ([]StoredBody, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, article_id, user_id, extractor_name, extractor_version, content_origin,
		       immutable, COALESCE(word_count, 0), extracted_at, is_current,
		       left(COALESCE(content_text, ''), $3)
		FROM article_content
		WHERE article_id = $2 AND (user_id = $1 OR user_id IS NULL)
		ORDER BY is_current DESC, extracted_at DESC, id DESC`,
		userID, id, bodyExcerptLength)
	if err != nil {
		return nil, fmt.Errorf("listing the bodies of article %d: %w", id, err)
	}
	defer rows.Close()

	var out []StoredBody
	for rows.Next() {
		var b StoredBody
		if err := rows.Scan(&b.ID, &b.ArticleID, &b.Owner, &b.ExtractorName, &b.ExtractorVersion,
			&b.ContentOrigin, &b.Immutable, &b.WordCount, &b.ExtractedAt, &b.Current,
			&b.Excerpt); err != nil {
			return nil, fmt.Errorf("scanning a stored body: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ErrNoSuchBody means the body named does not belong to the article named.
var ErrNoSuchBody = errors.New("no such body for this article")

// PromoteBody makes one stored body the one the reader sees.
//
// This is the deliberate human act the archive's rules are written around. An
// imported body is immutable and wins over everything automatically, because it may
// be the only surviving copy of a page that is gone — so nothing *automatic* may
// ever replace it. That leaves exactly one way for a better body to win: somebody
// looks at both and says so. Without this, an article imported with a thin copy of a
// page the archive has since fetched properly is stuck with the thin one forever.
//
// Both statements run in one transaction. The partial unique index permits one
// current body per article, so demoting and promoting are two halves of one change
// and a failure between them would leave an article with no body at all.
//
// Promotion does not change what a body *is*: an immutable body demoted here is
// still immutable, still never regenerated, and still there to be promoted back.
// What changes is which one is shown — and, as a consequence, whether the article is
// a candidate for re-extraction again, since that selects on the current body.
func (s *Store) PromoteBody(ctx context.Context, userID UserID, articleID ArticleID, bodyID ContentID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("promoting a body: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Checked against the article *and* against what this reader may choose from,
	// because the id arrives from a form. Promoting one article's body onto another
	// would show a reader a page they never asked for and would be invisible
	// afterwards; promoting another reader's would be worse, since it would confirm
	// that reader has one.
	var belongs bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM article_content
			 WHERE id = $1 AND article_id = $2 AND (user_id = $3 OR user_id IS NULL)
		)`, bodyID, articleID, userID).Scan(&belongs); err != nil {
		return fmt.Errorf("checking body %d against article %d: %w", bodyID, articleID, err)
	}
	if !belongs {
		// Not-found, never forbidden — the rule every other lookup here follows.
		return ErrNoSuchBody
	}

	// Promotion copies the chosen body into this reader's slot rather than moving
	// the row.
	//
	// This is the whole tenancy decision in one statement. Flipping is_current on
	// the row itself is what the single-user version did, and with two readers it
	// changes what everyone sees — including stranding another reader's highlights,
	// which anchor by quoted text rather than by body id. A copy costs one row and
	// decides only for the person who asked.
	//
	// It also leaves the household's own body untouched and promotable again, so
	// "put it back" is the same operation in the other direction.
	if _, err := tx.Exec(ctx, `
		UPDATE article_content SET is_current = false
		WHERE article_id = $1 AND is_current AND user_id IS NOT DISTINCT FROM $2`,
		articleID, userID); err != nil {
		return fmt.Errorf("demoting the reader's current body of article %d: %w", articleID, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO article_content (
			article_id, user_id, extractor_name, extractor_version, content_origin,
			immutable, content_html, content_text, word_count, is_current
		)
		SELECT article_id, $2, extractor_name, extractor_version, content_origin,
		       immutable, content_html, content_text, word_count, true
		FROM article_content WHERE id = $1`, bodyID, userID); err != nil {
		return fmt.Errorf("promoting body %d for reader %d: %w", bodyID, userID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("promoting a body: %w", err)
	}
	return nil
}
