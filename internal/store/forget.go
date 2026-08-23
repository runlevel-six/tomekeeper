package store

import (
	"context"
	"fmt"
	"time"
)

// Forgotten reports what one sweep of a reader's retention window did.
type Forgotten struct {
	// Tombstoned is the number of associations reduced to "finished with it" —
	// articles the reader can still reach through a subscription, where the link
	// between them exists anyway.
	Tombstoned int

	// Deleted is the number removed outright: articles reachable only through this
	// reader's own state, where the row was the last thing referring to them.
	Deleted int
}

// ForgetReadArticles lets one reader's association with old reading lapse.
//
// The reader's retention window is what says "old". When it passes, the article
// leaves their lists and their record of having read it goes — which is the point:
// this is a privacy setting, not a disk one. The article itself stays until every
// reader has reached the same point, because it belongs to the household.
//
// Three things are never forgotten, and they are the three ways a reader says they
// still want something:
//
//   - Starred, kept, or saved. The same exclusions expiry already honors.
//   - Highlighted. Annotations are the one thing a reader may value more than the
//     article, and deleting them on a timer is not a trade anybody asked for. An
//     article with a highlight is simply never a candidate.
//   - Not read, or read at an unknown time. Every ambiguity resolves toward keeping,
//     as it does in ExpirableArticles: a NULL read_at is possible for anything
//     marked read before that column existed.
//
// Whether the row is tombstoned or deleted turns on whether the reader could reach
// the article again anyway. If a feed of theirs carries it, the association is
// already recorded in feed_items and a tombstone reveals nothing further — while a
// deletion would make them look like a subscriber who has never opened it, which
// blocks expiry forever. If nothing but their own state refers to it, the row goes
// and the article is left referenced by nobody, which is what `tome prune` collects.
func (s *Store) ForgetReadArticles(
	ctx context.Context, userID UserID, cutoff time.Time, limit int,
) (Forgotten, error) {
	if limit <= 0 {
		limit = 100
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Forgotten{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// One candidate list, used by both writes below, so the two cannot disagree
	// about which articles this sweep is forgetting.
	rows, err := tx.Query(ctx, `
		SELECT st.article_id,
		       EXISTS (
		         SELECT 1 FROM feed_items fi
		         JOIN feeds f ON f.id = fi.feed_id
		         WHERE fi.article_id = st.article_id AND f.user_id = $1
		       ) AS reachable
		FROM article_state st
		WHERE st.user_id = $1
		  AND st.forgotten_at IS NULL
		  AND st.read AND NOT st.starred AND NOT st.kept AND st.saved_at IS NULL
		  AND st.read_at IS NOT NULL AND st.read_at < $2
		  AND NOT EXISTS (
		    SELECT 1 FROM highlights h
		    WHERE h.article_id = st.article_id AND h.user_id = $1
		  )
		ORDER BY st.read_at
		LIMIT $3`, userID, cutoff, limit)
	if err != nil {
		return Forgotten{}, fmt.Errorf("finding what user %d may forget: %w", userID, err)
	}

	var tombstone, remove []ArticleID
	for rows.Next() {
		var (
			id        ArticleID
			reachable bool
		)
		if err := rows.Scan(&id, &reachable); err != nil {
			rows.Close()
			return Forgotten{}, fmt.Errorf("scanning a forgettable article: %w", err)
		}
		if reachable {
			tombstone = append(tombstone, id)
		} else {
			remove = append(remove, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Forgotten{}, fmt.Errorf("reading forgettable articles: %w", err)
	}

	var out Forgotten

	if len(tombstone) > 0 {
		// Everything the reader might not want kept is cleared in the same
		// statement that marks the row finished, so there is no window in which a
		// tombstone still carries a read time.
		tag, err := tx.Exec(ctx, `
			UPDATE article_state
			SET forgotten_at = now(), read_at = NULL, read = true,
			    starred = false, kept = false, saved_at = NULL
			WHERE user_id = $1 AND article_id = ANY($2)`, userID, tombstone)
		if err != nil {
			return Forgotten{}, fmt.Errorf("forgetting articles for user %d: %w", userID, err)
		}
		out.Tombstoned = int(tag.RowsAffected())
	}

	if len(remove) > 0 {
		tag, err := tx.Exec(ctx,
			`DELETE FROM article_state WHERE user_id = $1 AND article_id = ANY($2)`,
			userID, remove)
		if err != nil {
			return Forgotten{}, fmt.Errorf("removing state for user %d: %w", userID, err)
		}
		out.Deleted = int(tag.RowsAffected())
	}

	if err := tx.Commit(ctx); err != nil {
		return Forgotten{}, err
	}
	return out, nil
}

// ReadersWithRetention lists readers who have asked to forget old reading, with
// the window each one wants.
//
// A reader with no setting of their own follows the archive's, which is why the
// household default is passed in rather than read here: it lives in configuration,
// not in the database, and a store method that reached for an environment variable
// would be the wrong shape.
//
// A zero window — theirs or the default — means keep everything, and such a reader
// is simply not returned. Retention stays off unless somebody turns it on.
func (s *Store) ReadersWithRetention(
	ctx context.Context, householdDefault time.Duration,
) (map[UserID]time.Duration, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, retain_after_read FROM users`)
	if err != nil {
		return nil, fmt.Errorf("listing retention settings: %w", err)
	}
	defer rows.Close()

	out := make(map[UserID]time.Duration)
	for rows.Next() {
		var (
			id     UserID
			window *time.Duration
		)
		if err := rows.Scan(&id, &window); err != nil {
			return nil, fmt.Errorf("scanning a retention setting: %w", err)
		}

		effective := householdDefault
		if window != nil {
			effective = *window
		}
		if effective > 0 {
			out[id] = effective
		}
	}
	return out, rows.Err()
}

// RetentionChoice is one option on the retention picker.
type RetentionChoice struct {
	// Value is what the form posts. Empty means "follow the archive's setting".
	Value string
	Name  string
	Blurb string
}

// RetentionChoices are the windows a reader may pick.
//
// A short list of round numbers rather than a free-text duration, because the
// difference between 45 and 60 days is not a decision anybody is really making and
// a text field invites a typo that silently deletes a year's reading.
//
// "Keep everything" is explicitly on the list and is not the same as leaving it
// unset: unset follows whatever the archive is configured to do, and keep-everything
// says no, whatever that is.
var RetentionChoices = []RetentionChoice{
	{Value: "", Name: "Whatever the archive does", Blurb: "The default for everybody here"},
	{Value: "0", Name: "Keep everything", Blurb: "Nothing is ever forgotten"},
	{Value: "720h", Name: "30 days", Blurb: "About a month"},
	{Value: "2160h", Name: "90 days", Blurb: "About three months"},
	{Value: "8760h", Name: "A year", Blurb: ""},
}

// RetentionFor parses a posted retention value.
//
// Assembled from the list above rather than parsed freely, so a hand-crafted POST
// cannot ask for a window nobody offered — the same rule the theme and cadence
// pickers follow, and it matters more here because the setting deletes things.
func RetentionFor(value string) (*time.Duration, bool) {
	for _, c := range RetentionChoices {
		if c.Value != value {
			continue
		}
		if c.Value == "" {
			return nil, true
		}
		d, err := time.ParseDuration(c.Value)
		if err != nil {
			return nil, false
		}
		return &d, true
	}
	return nil, false
}

// RetentionValue is the form value for a stored window, so the picker shows what
// is stored rather than a default that happens to look the same.
func RetentionValue(d *time.Duration) string {
	if d == nil {
		return ""
	}
	for _, c := range RetentionChoices {
		if c.Value == "" {
			continue
		}
		if parsed, err := time.ParseDuration(c.Value); err == nil && parsed == *d {
			return c.Value
		}
	}
	// A window set by hand that is not on the list. Reported as itself rather than
	// silently shown as one of the offered options.
	return d.String()
}
