package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Feed is a row of the feeds table. Feeds are user-scoped: every method here
// takes a UserID, and there is no variant that omits it.
type Feed struct {
	ID       FeedID
	UserID   UserID
	FeedURL  string
	SiteURL  string
	Title    string
	Category string

	// CategoryID is the folder's row, zero for a feed filed nowhere. Carried
	// alongside the name because the Fever API needs an id that survives a rename —
	// which is the reason categories became a table at all.
	CategoryID          CategoryID
	ETag                string
	LastModified        string
	PollInterval        time.Duration
	NextPollAt          time.Time
	LastPolledAt        *time.Time
	LastSuccessAt       *time.Time
	ConsecutiveFailures int
	LastError           string
	Disabled            bool

	// PollIntervalOverride is the cadence the reader chose for this feed alone, or
	// nil. Distinct from PollInterval, which is what the poller has learned and
	// rewrites on every poll.
	PollIntervalOverride *time.Duration

	// DefaultPollInterval is the same reader's general preference, carried on every
	// feed they own so that resolving "how often should this be checked" needs one
	// row rather than a second query per poll. Nil when they have not set one.
	//
	// Not a column on feeds: it comes from users, and it is read here because the
	// answer is a property of the pair.
	DefaultPollInterval *time.Duration
}

// ChosenInterval is the cadence the reader asked for, and whether they asked at
// all.
//
// The per-feed override wins over the general preference, which is the whole point
// of having both: the preference is what to do with seventy feeds, and the override
// is the one feed it is wrong for.
func (f Feed) ChosenInterval() (time.Duration, bool) {
	switch {
	case f.PollIntervalOverride != nil:
		return *f.PollIntervalOverride, true
	case f.DefaultPollInterval != nil:
		return *f.DefaultPollInterval, true
	default:
		return 0, false
	}
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
//
// The two cadence columns are read as nullable seconds, NULL meaning "no choice
// was made" in both cases. The reader's general preference is a correlated subquery
// rather than a join, so that this list needs only the one join below.

// feedFrom is the shared FROM clause that goes with feedColumns.
//
// They are separate constants that must be used together: feedColumns reads the
// category *name* through this join, because feeds.category is frozen at the 00013
// backfill and a query without the join would return a stale folder name that
// renames and refiles never touch. Naming the join here rather than repeating it at
// five call sites is the only thing stopping one of them from being forgotten.
const feedFrom = ` FROM feeds f LEFT JOIN categories c ON c.id = f.category_id `

const feedColumns = `
	f.id, f.user_id, f.feed_url, COALESCE(f.site_url, ''), f.title, COALESCE(c.name, ''), COALESCE(f.category_id, 0),
	COALESCE(f.etag, ''), COALESCE(f.last_modified, ''),
	EXTRACT(EPOCH FROM f.poll_interval)::bigint,
	f.next_poll_at, f.last_polled_at, f.last_success_at,
	f.consecutive_failures, COALESCE(f.last_error, ''), f.disabled,
	EXTRACT(EPOCH FROM f.poll_interval_override)::bigint,
	EXTRACT(EPOCH FROM (SELECT u.default_poll_interval FROM users u
	                     WHERE u.id = f.user_id))::bigint`

func scanFeed(row pgx.Row) (Feed, error) {
	var (
		f              Feed
		intervalSecond int64
		overrideSecond *int64
		defaultSecond  *int64
	)
	err := row.Scan(&f.ID, &f.UserID, &f.FeedURL, &f.SiteURL, &f.Title, &f.Category, &f.CategoryID,
		&f.ETag, &f.LastModified, &intervalSecond, &f.NextPollAt, &f.LastPolledAt,
		&f.LastSuccessAt, &f.ConsecutiveFailures, &f.LastError, &f.Disabled,
		&overrideSecond, &defaultSecond)
	if err != nil {
		return Feed{}, err
	}
	f.PollInterval = time.Duration(intervalSecond) * time.Second
	f.PollIntervalOverride = secondsToDuration(overrideSecond)
	f.DefaultPollInterval = secondsToDuration(defaultSecond)
	return f, nil
}

// secondsToDuration converts a nullable interval-as-seconds into an optional
// duration, keeping NULL and "no choice" the same thing all the way up.
func secondsToDuration(seconds *int64) *time.Duration {
	if seconds == nil {
		return nil
	}
	d := time.Duration(*seconds) * time.Second
	return &d
}

// durationSeconds is the reverse, for the parameter make_interval takes. Nil in,
// nil out, so an absent choice is stored as NULL rather than as zero.
func durationSeconds(d *time.Duration) *float64 {
	if d == nil {
		return nil
	}
	seconds := d.Seconds()
	return &seconds
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

	// An OPML folder name becomes a category the same way a typed one does. This is
	// how a re-import keeps working: folder names arrive as strings and always have.
	categoryID, err := s.ensureCategory(ctx, userID, p.Category)
	if err != nil {
		return 0, false, err
	}

	var (
		id      FeedID
		created bool
	)
	err = s.pool.QueryRow(ctx, `
		INSERT INTO feeds (user_id, feed_url, site_url, title, category_id)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5)
		ON CONFLICT (user_id, feed_url) DO UPDATE SET
			title    = EXCLUDED.title,
			site_url = COALESCE(EXCLUDED.site_url, feeds.site_url),
			-- COALESCE, so a re-import that omits a folder does not un-file a feed
			-- the reader has since moved. Unchanged in meaning from when this was a
			-- string; the value is now an id.
			category_id = COALESCE(EXCLUDED.category_id, feeds.category_id)
		RETURNING id, (xmax = 0)`,
		userID, p.FeedURL, p.SiteURL, p.Title, categoryID,
	).Scan(&id, &created)
	if err != nil {
		return 0, false, fmt.Errorf("upserting feed %s: %w", p.FeedURL, err)
	}
	return id, created, nil
}

// FeedEdit is the part of a subscription a reader may change by hand.
//
// Deliberately not FeedParams. That type is what an import supplies, and its
// upsert preserves whatever it does not carry — which is right for re-importing an
// OPML file and wrong for an edit, where emptying the category is a thing somebody
// means. SiteURL is absent for the same reason: it is read out of the feed rather
// than typed.
type FeedEdit struct {
	FeedURL  string
	Title    string
	Category string
	Disabled bool

	// PollInterval is how often the reader wants this one feed checked, and nil is
	// a real value: it means "however often you think", which is both the default
	// and the way back to it.
	PollInterval *time.Duration
}

// ErrFeedURLTaken means an edit would move a feed onto an address the same reader
// is already subscribed to.
//
// Typed rather than a message, because it is the one failure here that is the
// reader's to fix and needs saying in their terms. The unique constraint is what
// detects it: looking first and updating afterwards is a race even for one person
// with two tabs open.
var ErrFeedURLTaken = errors.New("already subscribed to that address")

// UpdateFeed applies an edit to one of a reader's subscriptions and returns the
// row as it now stands.
//
// The polling state is not simply left alone, because two of these edits change
// what polling means:
//
//   - A new address invalidates the conditional-GET validators, and a new host
//     invalidates the site URL with them. An ETag issued by the old endpoint means
//     nothing to the new one, and sending it invites a 304 for content that has
//     never been seen — which presents as a feed that looks healthy and produces
//     nothing.
//   - Re-enabling a feed has to clear the failure count, or the next single failure
//     re-crosses the threshold and disables it again immediately.
//
// Both also mean poll it now: in either case the reader has just done the thing
// that was meant to fix it and is waiting to find out whether it worked. Turning a
// feed off is the case that keeps its history — the count and the last error are
// still there to read when somebody comes back to the row.
//
// A cadence has its own, gentler effect on the schedule. Shortening one has to move
// the next poll or the choice does not take effect until the poll it was meant to
// change — up to a day away, which presents as a setting that did nothing. Moving
// it to `now()` would be the wrong correction, though: that is a refresh, and it
// would poll a feed fetched a minute ago. So the poll is brought forward only as
// far as the new cadence allows, measured from the last one — which is where the
// schedule would already be if the choice had been made a poll earlier.
func (s *Store) UpdateFeed(ctx context.Context, userID UserID, feedID FeedID, e FeedEdit) (Feed, error) {
	if e.FeedURL == "" {
		return Feed{}, fmt.Errorf("feed URL must not be empty")
	}
	if e.Title == "" {
		e.Title = e.FeedURL
	}

	// Every CASE below reads the row as it was: in an UPDATE, a bare column
	// reference on the right-hand side is the old value, so `feed_url = $3` asks
	// "is the address unchanged" without a second query to find out.
	// The form offers a name, not an id, and typing a new one creates the folder —
	// which is how this behaved when a category was free text, and the ergonomics
	// worth keeping. Resolved before the update rather than inside it because an
	// UPDATE cannot conjure a row in another table; a folder created for an update
	// that then fails is left behind empty, which is harmless now that an empty
	// category is a thing that can exist.
	categoryID, err := s.ensureCategory(ctx, userID, e.Category)
	if err != nil {
		return Feed{}, err
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE feeds AS f SET
			feed_url = $3,
			title    = $4,
			category_id = $5,
			disabled = $6,
			etag          = CASE WHEN feed_url = $3 THEN etag          ELSE NULL END,
			last_modified = CASE WHEN feed_url = $3 THEN last_modified ELSE NULL END,
			-- site_url is the base relative entry links are resolved against, and
			-- nothing but an import ever writes it. A feed that moves to another host
			-- has taken its site with it, so keeping the old one would resolve every
			-- relative link against a site this feed no longer belongs to. NULL is the
			-- honest answer — the poller falls back to the feed's own address — and the
			-- comparison is scheme-and-host, so correcting a path keeps it.
			site_url = CASE WHEN substring(feed_url from '^[^:]+://[^/]+')
			                   IS NOT DISTINCT FROM substring($3 from '^[^:]+://[^/]+')
			                THEN site_url ELSE NULL END,
			consecutive_failures = CASE WHEN feed_url <> $3 OR (disabled AND NOT $6)
			                            THEN 0 ELSE consecutive_failures END,
			last_error           = CASE WHEN feed_url <> $3 OR (disabled AND NOT $6)
			                            THEN NULL ELSE last_error END,
			poll_interval_override = make_interval(secs => $7::float8),
			-- least() ignores NULLs rather than returning one, which is what makes
			-- the second branch a no-op when the cadence is automatic: there is no
			-- bound to bring the poll forward to, so the schedule stands. Same
			-- expression for a cadence that did not change — it recomputes the bound
			-- the schedule is already under.
			next_poll_at         = CASE WHEN feed_url <> $3 OR (disabled AND NOT $6)
			                            THEN now()
			                            ELSE least(next_poll_at,
			                                       COALESCE(last_polled_at, now())
			                                         + make_interval(secs => $7::float8))
			                       END
		WHERE f.id = $1 AND f.user_id = $2`,
		feedID, userID, e.FeedURL, e.Title, categoryID, e.Disabled,
		durationSeconds(e.PollInterval))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return Feed{}, fmt.Errorf("moving feed %d to %s: %w", feedID, e.FeedURL, ErrFeedURLTaken)
		}
		return Feed{}, fmt.Errorf("updating feed %d: %w", feedID, err)
	}
	// Scoped to the reader, so a feed belonging to somebody else updates no rows and
	// is reported as not found — the same answer GetFeed gives, and for the same
	// reason.
	if tag.RowsAffected() == 0 {
		return Feed{}, pgx.ErrNoRows
	}

	// Read back rather than returned by the update: the category *name* comes from a
	// join, and a RETURNING clause has no FROM to join to. One column list is worth
	// more than one round trip on a form submit.
	return s.GetFeed(ctx, userID, feedID)
}

// ensureCategory turns a folder name into a row, creating it if the name is new.
//
// An empty name means no category, which is a NULL rather than a row — see the 00013
// migration for why "no folder" must not be a folder. Existing names are reused
// rather than duplicated, which the unique constraint would refuse anyway; doing the
// lookup here means a reader retyping a name they already have does not see an error
// about it.
func (s *Store) ensureCategory(ctx context.Context, userID UserID, name string) (*CategoryID, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}

	var id CategoryID
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO categories (user_id, name) VALUES ($1, $2)
		ON CONFLICT (user_id, name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, userID, name).Scan(&id); err != nil {
		return nil, fmt.Errorf("filing under category %q: %w", name, err)
	}
	return &id, nil
}

// uniqueViolation is SQLSTATE 23505. Named because a bare string at the comparison
// reads like a magic number, which is exactly what it is not.
const uniqueViolation = "23505"

// GetFeed returns one of the user's feeds.
//
// A feed belonging to another user is reported as not found rather than as a
// permission error: whether a given feed id exists at all is itself
// information about another user's subscriptions.
func (s *Store) GetFeed(ctx context.Context, userID UserID, feedID FeedID) (Feed, error) {
	f, err := scanFeed(s.pool.QueryRow(ctx,
		`SELECT `+feedColumns+feedFrom+`WHERE f.id = $1 AND f.user_id = $2`,
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
		`SELECT `+feedColumns+feedFrom+`WHERE f.user_id = $1 AND f.feed_url = $2`,
		userID, feedURL))
	if err != nil {
		return Feed{}, fmt.Errorf("looking up feed %q for user %d: %w", feedURL, userID, err)
	}
	return f, nil
}

// ListFeeds returns all of a user's feeds, ordered for display.
func (s *Store) ListFeeds(ctx context.Context, userID UserID) ([]Feed, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+feedColumns+feedFrom+`WHERE f.user_id = $1 ORDER BY c.name NULLS FIRST, f.title`,
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

// Category is one of a reader's folders, with enough counts to be worth showing in
// a list.
//
// Categories became a table in migration 00013. They were the `category` column on
// feeds until then — a category existing exactly as long as some feed claimed it,
// which made creating an empty one impossible and renaming one a rewrite of every
// feed in it. The reason for the change was not the interface: the Fever group id
// was a hash of the *name*, so renaming a category silently reshuffled a client's
// folders.
type Category struct {
	// ID is the row. Zero for the nameless bucket, which is the absence of a
	// category rather than a category named for absence — nothing can rename or
	// delete "no folder".
	ID CategoryID

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
	// Driven from categories rather than from feeds, which is the whole point of the
	// table: a folder with nothing in it has to appear, or "create a category, then
	// move feeds into it" is a sequence the reader cannot perform. A FULL JOIN, so
	// the nameless bucket — which has no row, by design — still arrives as a group.
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(c.id, 0), COALESCE(c.name, ''),
		       count(DISTINCT f.id),
		       count(DISTINCT a.id) FILTER (WHERE NOT COALESCE(st.read, false))
		FROM categories c
		  FULL JOIN feeds f ON f.category_id = c.id AND f.user_id = $1
		  LEFT JOIN feed_items fi ON fi.feed_id = f.id
		  LEFT JOIN articles a ON a.id = fi.article_id
		  LEFT JOIN article_state st ON st.article_id = a.id AND st.user_id = $1
		WHERE (c.user_id = $1 OR c.id IS NULL)
		  AND (f.user_id = $1 OR f.id IS NULL)
		GROUP BY c.id, c.name
		-- Empty last: a category with a name is a decision the reader made, and
		-- the leftovers belong at the bottom of the list rather than the top.
		HAVING c.id IS NOT NULL OR count(f.id) > 0
		ORDER BY (COALESCE(c.name, '') = ''), COALESCE(c.name, '')`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("listing categories for user %d: %w", userID, err)
	}
	defer rows.Close()

	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.Feeds, &c.Unread); err != nil {
			return nil, fmt.Errorf("scanning a category: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FeedRemoval is what unsubscribing from one feed would cost.
//
// Two numbers rather than one, because they answer different questions and only the
// second is a loss. Articles is the scale of the subscription. Stranded is how much of
// it would stop being reachable.
type FeedRemoval struct {
	// Articles is how many articles this feed carried.
	Articles int64

	// Stranded is how many of them would leave the reader's lists.
	//
	// An article stays visible when another of their feeds also carries it — feeds
	// are deduplicated onto shared articles — or when they have touched it at all,
	// because reading, starring or saving writes an `article_state` row and that row
	// is the second half of the visibility predicate. What is left is the articles
	// this feed alone introduced and the reader never opened: still on disk, still in
	// the archive directory, but no longer in any list. That is worth saying out loud
	// before somebody presses the button, and it is usually zero.
	Stranded int64
}

// FeedRemoval reports what unsubscribing from one of a reader's feeds would cost.
func (s *Store) FeedRemoval(ctx context.Context, userID UserID, feedID FeedID) (FeedRemoval, error) {
	var out FeedRemoval
	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (
		         WHERE NOT EXISTS (
		                 SELECT 1 FROM feed_items fi2
		                   JOIN feeds f2 ON f2.id = fi2.feed_id
		                  WHERE fi2.article_id = fi.article_id
		                    AND f2.user_id = $1 AND f2.id <> $2)
		           AND NOT EXISTS (
		                 SELECT 1 FROM article_state st
		                  WHERE st.article_id = fi.article_id AND st.user_id = $1))
		FROM feed_items fi
		WHERE fi.feed_id = $2
		  -- Scoped even though the caller has already looked the feed up: a count is
		  -- information, and the rule here is that no query answers a question about
		  -- another reader's archive.
		  AND EXISTS (SELECT 1 FROM feeds f WHERE f.id = $2 AND f.user_id = $1)`,
		userID, feedID,
	).Scan(&out.Articles, &out.Stranded)
	if err != nil {
		return FeedRemoval{}, fmt.Errorf("measuring the removal of feed %d: %w", feedID, err)
	}
	return out, nil
}

// DeleteFeed unsubscribes a reader from one feed, reporting whether there was one to
// remove.
//
// It deletes the subscription and its `feed_items` — which cascade — and no articles.
// That is the whole design of this archive in one statement: articles are the root
// entity, not children of a subscription, so a feed that turns bad or goes away must
// not be able to take what it delivered with it. An article this feed introduced keeps
// its stored bodies, its images on disk, its tags and its highlights.
//
// What it does affect is reachability, which FeedRemoval measures beforehand: an
// article nothing else carries and the reader never touched leaves their lists. It is
// still exportable and still in the archive directory; it is simply no longer
// referenced by anything the interface lists.
//
// Reports false rather than an error for a feed that is not this reader's, which is
// how a stale link becomes a 404 instead of a lie.
func (s *Store) DeleteFeed(ctx context.Context, userID UserID, feedID FeedID) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM feeds WHERE id = $1 AND user_id = $2`, feedID, userID)
	if err != nil {
		return false, fmt.Errorf("deleting feed %d: %w", feedID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListCategoryNames is the folder names a reader's feeds are filed under, and
// nothing else.
//
// Separate from ListCategories because the counts are the expensive half: this
// reads `feeds` alone, bounded by how many subscriptions somebody has, while the
// counts join feed_items, articles and article_state, which grows with the archive
// forever. Measured against a 1,900-article archive: 0.15ms here against 2.7ms
// there, and only the second figure gets worse over the years.
//
// That gap is the whole reason this exists. The category control it fills is drawn
// on the unread list, which is the most-requested page in the interface, and paying
// an archive-sized aggregate on every load to render eight words would be the wrong
// trade.
//
// An empty string in the result is a real entry: it means some feed carries no
// category, which is a bucket a reader can ask for.
func (s *Store) ListCategoryNames(ctx context.Context, userID UserID) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name FROM (
		    -- Every named folder the reader has, whether or not a feed is in it: an
		    -- empty one has to be offerable in a picker, or it can never be filled.
		    SELECT name FROM categories WHERE user_id = $1
		    UNION
		    -- And the nameless bucket, but only when something is actually in it.
		    -- There is no row for it by design, so it cannot be listed from the table.
		    SELECT '' FROM feeds
		    WHERE user_id = $1 AND category_id IS NULL
		) names
		-- Empty last, the same order ListCategories uses, so the compact control and
		-- the category index cannot disagree about which comes first.
		ORDER BY (name = ''), name`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing category names for user %d: %w", userID, err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scanning a category name: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// FeedsInCategory lists the feeds filed under one category, so a category's
// stream can say what it is made of. An empty name selects the feeds with no
// category.
func (s *Store) FeedsInCategory(ctx context.Context, userID UserID, category string) ([]Feed, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+feedColumns+`
		 `+feedFrom+`
		 WHERE f.user_id = $1 AND COALESCE(c.name, '') = $2
		 ORDER BY f.title`,
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
