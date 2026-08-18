package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// OriginImport builds the content_origin for a body that came from another
// system: "import:wallabag".
//
// Prefixed rather than a bare source name so that provenance is answerable with a
// LIKE and so that no future feed or extractor can collide with an importer's
// name. Recorded per body rather than per article, because one article can hold an
// imported body and a fetched one at the same time — which is precisely what
// happens when the archive later fetches a page it was given a copy of.
func OriginImport(sourceName string) string { return "import:" + sourceName }

// ImportHighlight is one passage a reader marked in the system being imported
// from.
type ImportHighlight struct {
	Quote     string
	Note      string
	CreatedAt *time.Time
}

// ImportParams is one article arriving from another system.
//
// The body is already extracted and already sanitized by the caller: this type is
// a description of what to write, and it is the importer's job to have made the
// HTML safe with the same policy the extraction ladder uses. Storing markup that
// has not been through it would put unsanitized third-party HTML into a body the
// reader renders as trusted.
type ImportParams struct {
	// SourceName and SourceID are the idempotency key, together with the user.
	// SourceID may be empty for a source that has no stable id, in which case the
	// canonical URL is what makes a re-import recognizable.
	SourceName string
	SourceID   string

	URLCanonical string
	URLOriginal  string

	Title       string
	Author      string
	SiteName    string
	Language    string
	PublishedAt *time.Time

	// SavedAt is when the source recorded the article as saved. It becomes the
	// reader's saved_at, so a ten-year library keeps its own chronology rather than
	// arriving all at once with today's date.
	SavedAt *time.Time

	// Extractor, ExtractorVersion and ContentOrigin describe where the body came
	// from, when the source knows. Empty means it does not, and the body is
	// recorded as this source's import — which is right for another system's
	// library and wrong for a restore of this archive's own export, where the body
	// may have been fetched and extracted here and must stay replaceable.
	//
	// Immutable is honored only alongside a stated ContentOrigin. An import that
	// does not say where its body came from is always immutable, because it may be
	// the only surviving copy of a dead URL.
	Extractor        string
	ExtractorVersion string
	ContentOrigin    string
	Immutable        bool

	// ContentHTML is empty when the source had no usable body — including when it
	// held a placeholder its own fetch left behind. Empty means the article is
	// stored with no body and left for this archive's own fetch to fill, which is
	// an improvement on the source rather than a loss.
	ContentHTML string
	ContentText string
	WordCount   int

	Read    bool
	Starred bool

	Tags       []string
	Highlights []ImportHighlight
}

// Imported reports what one record's import did.
type Imported struct {
	ArticleID ArticleID

	// AlreadyImported is true when this source record had been imported before.
	// The write is then confined to what a re-import may safely add: tags and
	// highlights that are new, and nothing else.
	AlreadyImported bool

	// ArticleExisted is true when the archive already held this URL — carried by a
	// feed, or saved by hand. The import becomes another reference to the same
	// article rather than a second copy of it.
	ArticleExisted bool

	// BodyStored is true when this call wrote the imported body.
	BodyStored bool

	TagsAdded       int
	HighlightsAdded int
}

// ImportArticle writes one article from another system, idempotently.
//
// Re-running an import is the ordinary way to recover from one that stopped
// halfway, so every step here is safe to repeat and the import record is written
// *last*. That ordering is the recovery property: a run interrupted between the
// article and its record leaves the record absent, and the next run finishes the
// job. A record written first would leave the article half-written and
// permanently claimed.
//
// What a re-import must never do, and the reason this is not simply an upsert of
// everything:
//
//   - It must not remove a tag the reader added here. Tags are additive.
//   - It must not duplicate highlights. They are matched on their quoted text.
//   - It must not take back read or starred state. Both are OR-ed with what the
//     reader already has, so a page read here after being imported unread stays
//     read, and re-importing cannot un-star anything.
//   - It must not write a second imported body. One per article per source.
func (s *Store) ImportArticle(ctx context.Context, userID UserID, p ImportParams) (Imported, error) {
	if p.SourceName == "" {
		return Imported{}, errors.New("an import needs a source name")
	}
	if p.URLCanonical == "" {
		return Imported{}, errors.New("an import needs a canonical URL")
	}

	var result Imported

	// Already imported? Answered before anything is written, because the answer
	// changes what this call is allowed to do.
	existing, found, err := s.ImportedArticle(ctx, userID, p.SourceName, p.SourceID)
	if err != nil {
		return Imported{}, err
	}
	result.AlreadyImported = found

	articleID := existing
	if !found {
		id, created, err := s.UpsertArticle(ctx, ArticleParams{
			URLCanonical: p.URLCanonical,
			URLOriginal:  p.URLOriginal,
			Title:        p.Title,
			Author:       p.Author,
			SiteName:     p.SiteName,
			Language:     p.Language,
			PublishedAt:  p.PublishedAt,
		})
		if err != nil {
			return Imported{}, fmt.Errorf("recording the imported article: %w", err)
		}
		articleID = id
		result.ArticleExisted = !created
	}
	result.ArticleID = articleID

	// The body, once. A second import of the same record must not stack another
	// content row, and an article that already carries this source's body has
	// nothing to gain from a second copy of it.
	if p.ContentHTML != "" {
		has, err := s.hasBodyFrom(ctx, articleID, p)
		if err != nil {
			return Imported{}, err
		}
		if !has {
			body := ContentParams{
				ArticleID: articleID,
				HTML:      p.ContentHTML,
				Text:      p.ContentText,
				WordCount: p.WordCount,

				ExtractorName:    p.Extractor,
				ExtractorVersion: p.ExtractorVersion,
				ContentOrigin:    p.ContentOrigin,
				Immutable:        p.Immutable,
			}
			if body.ContentOrigin == "" {
				// A source that does not say where its body came from. Recorded as this
				// source's import, and immutable: it may be the only surviving copy of a
				// page that no longer exists, so it is never demoted by a later
				// extraction and never selected by reextract. A fetched body is stored
				// alongside it, and promoting one is a deliberate act.
				//
				// A source that *does* say — this archive's own export — is reproduced
				// as it was, because a restore has to give back the archive rather than
				// convert every body in it into an unreplaceable import.
				body.ExtractorName = "imported"
				body.ExtractorVersion = p.SourceName
				body.ContentOrigin = OriginImport(p.SourceName)
				body.Immutable = true
			}

			if _, err := s.InsertContent(ctx, body); err != nil {
				return Imported{}, fmt.Errorf("storing the imported body: %w", err)
			}
			result.BodyStored = true
		}
	}

	if err := s.applyImportedState(ctx, userID, articleID, p); err != nil {
		return Imported{}, err
	}

	for _, name := range p.Tags {
		tagID, err := s.EnsureTag(ctx, userID, name)
		if err != nil {
			return Imported{}, fmt.Errorf("recording the imported tag %q: %w", name, err)
		}
		added, err := s.TagArticle(ctx, userID, articleID, tagID)
		if err != nil {
			return Imported{}, fmt.Errorf("tagging the imported article: %w", err)
		}
		if added {
			result.TagsAdded++
		}
	}

	for _, h := range p.Highlights {
		added, err := s.AddHighlight(ctx, userID, articleID, h)
		if err != nil {
			return Imported{}, err
		}
		if added {
			result.HighlightsAdded++
		}
	}

	// Last, and only now: the marker that says this record is done.
	if err := s.recordImport(ctx, userID, p.SourceName, p.SourceID, articleID); err != nil {
		return Imported{}, err
	}

	return result, nil
}

// applyImportedState gives the reader their relationship to an imported article.
//
// saved_at is what puts an import in the reading list, and it is also what keeps
// it out of the retention policy's reach: a saved article is claimed forever, which
// is the correct answer for a body that may be the only copy left. COALESCEd, so
// re-importing does not reset when the reader first kept it, and neither does
// importing something they had already saved by hand.
//
// read and starred are OR-ed rather than assigned. The source's answer is a
// starting point, not an authority: an article imported unread and then read here
// must not revert on the next run, and a feed may already have led the reader to
// read it before the import ever happened.
func (s *Store) applyImportedState(ctx context.Context, userID UserID, id ArticleID, p ImportParams) error {
	savedAt := p.SavedAt
	if savedAt == nil {
		now := time.Now().UTC()
		savedAt = &now
	}

	// The timestamps are cast explicitly. Inside a CASE, Postgres has nothing to
	// infer a bare parameter's type from and settles on text, which fails against a
	// timestamptz column — and fails for every record at once, which is at least a
	// loud way to find out.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO article_state (user_id, article_id, read, read_at, starred, saved_at)
		SELECT $1, a.id, $3, CASE WHEN $3 THEN $6::timestamptz END, $4, $5::timestamptz
		FROM articles a
		WHERE a.id = $2
		ON CONFLICT (user_id, article_id) DO UPDATE
		SET read     = article_state.read OR EXCLUDED.read,
		    starred  = article_state.starred OR EXCLUDED.starred,
		    saved_at = COALESCE(article_state.saved_at, EXCLUDED.saved_at),
		    read_at  = CASE
		        WHEN article_state.read OR EXCLUDED.read
		        THEN COALESCE(article_state.read_at, EXCLUDED.read_at)
		        ELSE NULL
		    END`,
		userID, id, p.Read, p.Starred, savedAt, savedAt); err != nil {
		return fmt.Errorf("recording the reader's state for imported article %d: %w", id, err)
	}
	return nil
}

// ImportedArticle looks up what a source record was imported as.
//
// Reports false rather than an error when the record has never been imported, and
// also when SourceID is empty: a source with no stable id has nothing to key on,
// and the canonical URL then does the deduplication instead — through
// UpsertArticle, which is where identity lives anyway.
func (s *Store) ImportedArticle(ctx context.Context, userID UserID, sourceName, sourceID string,
) (ArticleID, bool, error) {
	if sourceID == "" {
		return 0, false, nil
	}

	var id ArticleID
	err := s.pool.QueryRow(ctx, `
		SELECT article_id FROM import_records
		WHERE user_id = $1 AND source_name = $2 AND source_id = $3`,
		userID, sourceName, sourceID).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("looking up import %s/%s: %w", sourceName, sourceID, err)
	}
	return id, true, nil
}

// ArticleVisibleByURL reports whether this reader already has an article at a
// canonical URL, and which one.
//
// User-scoped on purpose, where GetArticleByURL is not. That method answers a
// question about the shared article pool and is used by the fetch pipeline, which
// has no user; this one answers "is this already in *your* archive", which is what
// an import report is claiming when it says a record duplicates something. The
// global answer would count an article another reader saved — a number that is
// wrong for the report and, said out loud, is one reader learning what another has.
func (s *Store) ArticleVisibleByURL(ctx context.Context, userID UserID, canonical string,
) (ArticleID, bool, error) {
	var id ArticleID
	err := s.pool.QueryRow(ctx, `
		SELECT a.id FROM articles a
		WHERE a.url_canonical = $2 AND `+visibleArticles,
		userID, canonical).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("looking up article %q for user %d: %w", canonical, userID, err)
	}
	return id, true, nil
}

// recordImport marks a source record as imported.
func (s *Store) recordImport(ctx context.Context, userID UserID,
	sourceName, sourceID string, id ArticleID,
) error {
	if sourceID == "" {
		// Nothing to key on. The article is still imported; it simply cannot be
		// recognized by id on a later run, and will be deduplicated by URL.
		return nil
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO import_records (user_id, source_name, source_id, article_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, source_name, source_id) DO UPDATE
		SET article_id = EXCLUDED.article_id`,
		userID, sourceName, sourceID, id); err != nil {
		return fmt.Errorf("recording the import of %s/%s: %w", sourceName, sourceID, err)
	}
	return nil
}

// hasBodyFrom reports whether an article already carries a body of this
// provenance, which is what keeps a repeated import from stacking content rows.
//
// Asked about the origin rather than about the source, because a restore states its
// own origins: re-running one must not add a second copy of a body that says it was
// fetched, any more than re-running a Wallabag import adds a second imported one.
func (s *Store) hasBodyFrom(ctx context.Context, id ArticleID, p ImportParams) (bool, error) {
	origin := p.ContentOrigin
	if origin == "" {
		origin = OriginImport(p.SourceName)
	}

	var exists bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM article_content
			WHERE article_id = $1 AND content_origin = $2)`,
		id, origin).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking for an existing body on article %d: %w", id, err)
	}
	return exists, nil
}

// AddHighlight stores a passage the reader marked, unless it is already there.
//
// Matched on the quoted text rather than on an id, because that is the only thing
// two systems can agree on: a highlight's identity in the source is a character
// range into the source's own rendering of the body, and this archive's body was
// produced by a different extractor. The text survives that; the offsets do not.
//
// Reports whether a row was written, so a re-import can say it added nothing
// rather than claiming to have restored what was already present.
func (s *Store) AddHighlight(ctx context.Context, userID UserID, id ArticleID, h ImportHighlight,
) (bool, error) {
	if h.Quote == "" {
		return false, nil
	}

	createdAt := h.CreatedAt
	if createdAt == nil {
		now := time.Now().UTC()
		createdAt = &now
	}

	// Guarded by the visibility predicate for the same reason state writes are: a
	// highlight against an arbitrary article id would let one reader confirm what
	// another has archived, one insert at a time.
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO highlights (user_id, article_id, quote, note, created_at)
		SELECT $1, a.id, $3, NULLIF($4, ''), $5
		FROM articles a
		WHERE a.id = $2 AND `+visibleArticles+`
		  AND NOT EXISTS (
		    SELECT 1 FROM highlights h
		    WHERE h.user_id = $1 AND h.article_id = a.id AND h.quote = $3)`,
		userID, id, h.Quote, h.Note, createdAt)
	if err != nil {
		return false, fmt.Errorf("storing a highlight on article %d: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// HighlightsForArticle lists a reader's highlights on one article.
func (s *Store) HighlightsForArticle(ctx context.Context, userID UserID, id ArticleID,
) ([]ImportHighlight, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT quote, COALESCE(note, ''), created_at
		FROM highlights
		WHERE user_id = $1 AND article_id = $2
		ORDER BY created_at, id`, userID, id)
	if err != nil {
		return nil, fmt.Errorf("listing highlights on article %d: %w", id, err)
	}
	defer rows.Close()

	var out []ImportHighlight
	for rows.Next() {
		var h ImportHighlight
		var createdAt time.Time
		if err := rows.Scan(&h.Quote, &h.Note, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning a highlight: %w", err)
		}
		h.CreatedAt = &createdAt
		out = append(out, h)
	}
	return out, rows.Err()
}

// ImportedCount is how many of a source's records this reader has imported.
type ImportedCount struct {
	SourceName string
	Articles   int64
}

// ImportCounts reports what has been imported, per source.
func (s *Store) ImportCounts(ctx context.Context, userID UserID) ([]ImportedCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source_name, count(*)
		FROM import_records
		WHERE user_id = $1
		GROUP BY source_name
		ORDER BY source_name`, userID)
	if err != nil {
		return nil, fmt.Errorf("counting imports for user %d: %w", userID, err)
	}
	defer rows.Close()

	var out []ImportedCount
	for rows.Next() {
		var c ImportedCount
		if err := rows.Scan(&c.SourceName, &c.Articles); err != nil {
			return nil, fmt.Errorf("scanning an import count: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
