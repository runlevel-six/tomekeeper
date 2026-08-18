package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Feed is a row of the feeds table. Feeds are user-scoped: every method here
// takes a UserID, and there is no variant that omits it.
type Feed struct {
	ID                  FeedID
	UserID              UserID
	FeedURL             string
	SiteURL             string
	Title               string
	Category            string
	ETag                string
	LastModified        string
	PollInterval        time.Duration
	NextPollAt          time.Time
	LastPolledAt        *time.Time
	LastSuccessAt       *time.Time
	ConsecutiveFailures int
	LastError           string
	Disabled            bool
}

// FeedParams is the subscription data an OPML file or a manual add supplies.
type FeedParams struct {
	FeedURL  string
	SiteURL  string
	Title    string
	Category string
}

// feedColumns is the shared SELECT list. Nullable text is coalesced to the
// empty string so that scanning never needs a pointer for a value that has no
// meaningful difference between NULL and "".
const feedColumns = `
	id, user_id, feed_url, COALESCE(site_url, ''), title, COALESCE(category, ''),
	COALESCE(etag, ''), COALESCE(last_modified, ''),
	EXTRACT(EPOCH FROM poll_interval)::bigint,
	next_poll_at, last_polled_at, last_success_at,
	consecutive_failures, COALESCE(last_error, ''), disabled`

func scanFeed(row pgx.Row) (Feed, error) {
	var (
		f              Feed
		intervalSecond int64
	)
	err := row.Scan(&f.ID, &f.UserID, &f.FeedURL, &f.SiteURL, &f.Title, &f.Category,
		&f.ETag, &f.LastModified, &intervalSecond, &f.NextPollAt, &f.LastPolledAt,
		&f.LastSuccessAt, &f.ConsecutiveFailures, &f.LastError, &f.Disabled)
	if err != nil {
		return Feed{}, err
	}
	f.PollInterval = time.Duration(intervalSecond) * time.Second
	return f, nil
}

// UpsertFeed adds a subscription for a user, or updates the title and category
// of one that already exists, reporting whether it was created.
//
// Re-importing the same OPML file must not create duplicates and must not
// disturb polling state, so etag, intervals, and failure counts are untouched
// on conflict.
func (s *Store) UpsertFeed(ctx context.Context, userID UserID, p FeedParams) (FeedID, bool, error) {
	if p.FeedURL == "" {
		return 0, false, fmt.Errorf("feed URL must not be empty")
	}
	if p.Title == "" {
		p.Title = p.FeedURL
	}

	var (
		id      FeedID
		created bool
	)
	err := s.pool.QueryRow(ctx, `
		INSERT INTO feeds (user_id, feed_url, site_url, title, category)
		VALUES ($1, $2, NULLIF($3, ''), $4, NULLIF($5, ''))
		ON CONFLICT (user_id, feed_url) DO UPDATE SET
			title    = EXCLUDED.title,
			site_url = COALESCE(EXCLUDED.site_url, feeds.site_url),
			category = COALESCE(EXCLUDED.category, feeds.category)
		RETURNING id, (xmax = 0)`,
		userID, p.FeedURL, p.SiteURL, p.Title, p.Category,
	).Scan(&id, &created)
	if err != nil {
		return 0, false, fmt.Errorf("upserting feed %s: %w", p.FeedURL, err)
	}
	return id, created, nil
}

// GetFeed returns one of the user's feeds.
//
// A feed belonging to another user is reported as not found rather than as a
// permission error: whether a given feed id exists at all is itself
// information about another user's subscriptions.
func (s *Store) GetFeed(ctx context.Context, userID UserID, feedID FeedID) (Feed, error) {
	f, err := scanFeed(s.pool.QueryRow(ctx,
		`SELECT `+feedColumns+` FROM feeds WHERE id = $1 AND user_id = $2`,
		feedID, userID))
	if err != nil {
		return Feed{}, fmt.Errorf("looking up feed %d: %w", feedID, err)
	}
	return f, nil
}

// FeedByURL looks up one of the reader's feeds by its URL.
//
// Reported as not found for a URL they do not subscribe to, and scoped to the
// reader for the usual reason: feeds are per-user rows, so "am I already
// subscribed to this" is a question about one person's list and not about the
// archive.
func (s *Store) FeedByURL(ctx context.Context, userID UserID, feedURL string) (Feed, error) {
	f, err := scanFeed(s.pool.QueryRow(ctx,
		`SELECT `+feedColumns+` FROM feeds WHERE user_id = $1 AND feed_url = $2`,
		userID, feedURL))
	if err != nil {
		return Feed{}, fmt.Errorf("looking up feed %q for user %d: %w", feedURL, userID, err)
	}
	return f, nil
}

// ListFeeds returns all of a user's feeds, ordered for display.
func (s *Store) ListFeeds(ctx context.Context, userID UserID) ([]Feed, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+feedColumns+` FROM feeds WHERE user_id = $1 ORDER BY category NULLS FIRST, title`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("listing feeds: %w", err)
	}
	defer rows.Close()

	var feeds []Feed
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning feed: %w", err)
		}
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

// RecordPollSuccess records a completed poll that returned content.
//
// The conditional-GET validators are overwritten with whatever the response
// carried, including with nothing: a server that stops sending ETags must not
// leave a stale one behind, or every subsequent request would send a validator
// the origin no longer recognizes.
func (s *Store) RecordPollSuccess(ctx context.Context, userID UserID, feedID FeedID,
	etag, lastModified string, interval time.Duration,
) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE feeds SET
			etag                 = NULLIF($3, ''),
			last_modified        = NULLIF($4, ''),
			poll_interval        = make_interval(secs => $5),
			next_poll_at         = now() + make_interval(secs => $5),
			last_polled_at       = now(),
			last_success_at      = now(),
			consecutive_failures = 0,
			last_error           = NULL
		WHERE id = $1 AND user_id = $2`,
		feedID, userID, etag, lastModified, interval.Seconds())
	if err != nil {
		return fmt.Errorf("recording poll success for feed %d: %w", feedID, err)
	}
	return nil
}

// RecordPollNotModified records a 304 response.
//
// This is the success case that costs nothing, and the acceptance criterion
// worth holding to is that a second poll of unchanged feeds produces mostly these.
func (s *Store) RecordPollNotModified(ctx context.Context, userID UserID, feedID FeedID,
	interval time.Duration,
) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE feeds SET
			poll_interval        = make_interval(secs => $3),
			next_poll_at         = now() + make_interval(secs => $3),
			last_polled_at       = now(),
			last_success_at      = now(),
			consecutive_failures = 0,
			last_error           = NULL
		WHERE id = $1 AND user_id = $2`,
		feedID, userID, interval.Seconds())
	if err != nil {
		return fmt.Errorf("recording 304 for feed %d: %w", feedID, err)
	}
	return nil
}

// RecordPollFailure records a failed poll and disables the feed once it has
// failed disableAfter times in a row. It reports whether the feed is now
// disabled.
//
// A disabled feed is never silently dropped: it keeps its last error, and the
// feed health view exists so that the failure is visible rather than a slow
// puncture in the archive.
func (s *Store) RecordPollFailure(ctx context.Context, userID UserID, feedID FeedID,
	cause string, interval time.Duration, disableAfter int,
) (bool, error) {
	var disabled bool
	err := s.pool.QueryRow(ctx, `
		UPDATE feeds SET
			poll_interval        = make_interval(secs => $5),
			next_poll_at         = now() + make_interval(secs => $5),
			last_polled_at       = now(),
			consecutive_failures = consecutive_failures + 1,
			last_error           = $3,
			disabled             = disabled OR (consecutive_failures + 1 >= $4)
		WHERE id = $1 AND user_id = $2
		RETURNING disabled`,
		feedID, userID, cause, disableAfter, interval.Seconds(),
	).Scan(&disabled)
	if err != nil {
		return false, fmt.Errorf("recording poll failure for feed %d: %w", feedID, err)
	}
	return disabled, nil
}

// SetFeedDisabled enables or disables a feed, clearing the failure count when
// it is re-enabled so that a fixed feed gets a fresh budget of attempts.
func (s *Store) SetFeedDisabled(ctx context.Context, userID UserID, feedID FeedID, disabled bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE feeds SET
			disabled             = $3,
			consecutive_failures = CASE WHEN $3 THEN consecutive_failures ELSE 0 END,
			last_error           = CASE WHEN $3 THEN last_error ELSE NULL END,
			next_poll_at         = CASE WHEN $3 THEN next_poll_at ELSE now() END
		WHERE id = $1 AND user_id = $2`,
		feedID, userID, disabled)
	if err != nil {
		return fmt.Errorf("setting feed %d disabled=%v: %w", feedID, disabled, err)
	}
	return nil
}

// Category is one of the folders an OPML import produced, with enough counts to
// be worth showing in a list.
//
// Categories are not a table. They are the `category` column on feeds, which is
// where an OPML folder name lands — so a category exists exactly as long as some
// feed claims it, and renaming one is a matter of re-importing. That is a
// deliberate choice over a `categories` table: the reader never creates one by
// hand, and a table would need reconciling against every import.
type Category struct {
	// Name is the folder name. Empty means these feeds carry no category, which
	// callers have to represent explicitly rather than as a missing value — see
	// StreamQuery.NoCategory.
	Name string

	Feeds  int64
	Unread int64
}

// ListCategories groups a user's feeds by category, newest-unread-first counts
// included.
//
// Unread is counted with DISTINCT over articles for the same reason
// UnreadCountsFor is: two feeds in one category can carry the same syndicated
// story, and it is one unread article to the reader.
func (s *Store) ListCategories(ctx context.Context, userID UserID) ([]Category, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(f.category, ''),
		       count(DISTINCT f.id),
		       count(DISTINCT a.id) FILTER (WHERE NOT COALESCE(st.read, false))
		FROM feeds f
		  LEFT JOIN feed_items fi ON fi.feed_id = f.id
		  LEFT JOIN articles a ON a.id = fi.article_id
		  LEFT JOIN article_state st ON st.article_id = a.id AND st.user_id = $1
		WHERE f.user_id = $1
		GROUP BY COALESCE(f.category, '')
		-- Empty last: a category with a name is a decision the reader made, and
		-- the leftovers belong at the bottom of the list rather than the top.
		ORDER BY (COALESCE(f.category, '') = ''), COALESCE(f.category, '')`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("listing categories for user %d: %w", userID, err)
	}
	defer rows.Close()

	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.Name, &c.Feeds, &c.Unread); err != nil {
			return nil, fmt.Errorf("scanning a category: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FeedsInCategory lists the feeds filed under one category, so a category's
// stream can say what it is made of. An empty name selects the feeds with no
// category.
func (s *Store) FeedsInCategory(ctx context.Context, userID UserID, category string) ([]Feed, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+feedColumns+`
		 FROM feeds
		 WHERE user_id = $1 AND COALESCE(category, '') = $2
		 ORDER BY title`,
		userID, category)
	if err != nil {
		return nil, fmt.Errorf("listing feeds in category %q: %w", category, err)
	}
	defer rows.Close()

	var feeds []Feed
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning feed: %w", err)
		}
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

// PollFloor is how recently a feed must have been polled for a manual refresh to
// leave it alone.
//
// The refresh button is one tap and there are dozens of feeds behind it, so
// without a floor an impatient reader is a small denial-of-service against
// everyone they subscribe to. Five minutes is short enough that "check now"
// still means now for any feed a reader is actually waiting on, and long enough
// that pressing the button repeatedly costs the origins nothing.
const PollFloor = 5 * time.Minute

// PollNowResult is what a manual refresh did.
type PollNowResult struct {
	// Moved is how many feeds are now due.
	Moved int64

	// Held is how many were polled inside PollFloor and left alone. Reported
	// rather than hidden: a reader who presses refresh and sees nothing change
	// deserves to know the request was understood and declined.
	Held int64

	// Disabled is how many feeds were skipped because they are disabled. A
	// refresh deliberately does not revive them — that is what the feed health
	// view's own control is for, and quietly re-enabling twenty dead feeds on a
	// reload would undo the auto-disable entirely.
	Disabled int64
}

// PollNow brings forward the next poll of every one of a user's enabled feeds,
// so the scheduler picks them up on its next pass.
//
// Deliberately a nudge to the schedule rather than an enqueue. `tome serve` has
// no job client — polling belongs to the worker, and giving the web process one
// would mean two processes able to insert feed polls, with the request handler
// the one holding a transaction open while an origin server thinks about it.
// The cost is that "now" means "within one scheduler pass", which the caller is
// expected to say out loud.
func (s *Store) PollNow(ctx context.Context, userID UserID) (PollNowResult, error) {
	// One statement rather than an update and then a count, so the three numbers
	// add up to the reader's subscription count and cannot disagree with each
	// other. The CTE and the outer query share a snapshot, and the update touches
	// only next_poll_at, so the filters below still see the pre-update state of
	// every column they read.
	var r PollNowResult
	err := s.pool.QueryRow(ctx, `
		WITH floor AS (SELECT now() - make_interval(secs => $2) AS at),
		moved AS (
			UPDATE feeds SET next_poll_at = now()
			WHERE user_id = $1
			  AND NOT disabled
			  AND (last_polled_at IS NULL OR last_polled_at < (SELECT at FROM floor))
			RETURNING 1
		)
		SELECT (SELECT count(*) FROM moved),
		       count(*) FILTER (WHERE NOT disabled
		                        AND last_polled_at >= (SELECT at FROM floor)),
		       count(*) FILTER (WHERE disabled)
		FROM feeds WHERE user_id = $1`,
		userID, PollFloor.Seconds()).Scan(&r.Moved, &r.Held, &r.Disabled)
	if err != nil {
		return PollNowResult{}, fmt.Errorf("bringing polls forward for user %d: %w", userID, err)
	}
	return r, nil
}

// DueFeed identifies a feed that is ready to be polled, together with the user
// it belongs to, so that the resulting job can be user-scoped again.
type DueFeed struct {
	FeedID FeedID
	UserID UserID
}

// DueFeeds returns feeds whose next poll time has passed, across all users.
//
// This is on SystemStore, not Store, because it deliberately ignores user
// scoping: the scheduler serves every user at once. It returns the owning
// UserID with each feed precisely so that everything downstream — the job
// args, and every write the poller performs — is scoped again.
//
// It must never be called from a request handler.
func (s *SystemStore) DueFeeds(ctx context.Context, limit int) ([]DueFeed, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id
		FROM feeds
		WHERE NOT disabled AND next_poll_at <= now()
		ORDER BY next_poll_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing due feeds: %w", err)
	}
	defer rows.Close()

	var due []DueFeed
	for rows.Next() {
		var d DueFeed
		if err := rows.Scan(&d.FeedID, &d.UserID); err != nil {
			return nil, fmt.Errorf("scanning due feed: %w", err)
		}
		due = append(due, d)
	}
	return due, rows.Err()
}
