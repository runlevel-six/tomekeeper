package store

import (
	"context"
	"fmt"
	"time"
)

// Preferences is everything the settings page stores about one reader.
//
// Read as a struct rather than a value at a time because every page needs all of
// it to draw the chrome: the palette goes into the first paint and the reading
// preferences decide which behavior the page's own script is allowed to have.
// One row, one round trip, however many preferences there come to be.
type Preferences struct {
	// Theme is the palette and light/dark choice, as one value. See themes.go.
	Theme string

	// MarkReadOnScroll is whether an unread list marks articles read as they are
	// scrolled past. Off unless the reader turned it on.
	MarkReadOnScroll bool

	// DefaultPollInterval is how often they want their feeds checked, and nil —
	// which is what everybody has until they say otherwise — means the poller
	// decides per feed. A feed with its own override does not consult this.
	DefaultPollInterval *time.Duration
}

// GetPreferences returns one reader's settings.
//
// A failure returns usable defaults alongside the error, so a caller that would
// rather draw the page than fail it — which is every caller — does not have to
// invent them. The default palette is 'auto' and automatic marking is off, which
// is what a reader who has never opened the settings page has.
func (s *Store) GetPreferences(ctx context.Context, userID UserID) (Preferences, error) {
	prefs := Preferences{Theme: "auto"}

	var pollSeconds *int64
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(theme, 'auto'), mark_read_on_scroll,
		       EXTRACT(EPOCH FROM default_poll_interval)::bigint
		FROM users WHERE id = $1`, userID,
	).Scan(&prefs.Theme, &prefs.MarkReadOnScroll, &pollSeconds); err != nil {
		return Preferences{Theme: "auto"}, fmt.Errorf("reading preferences for user %d: %w", userID, err)
	}
	prefs.DefaultPollInterval = secondsToDuration(pollSeconds)
	return prefs, nil
}

// SetDefaultPollInterval stores how often the reader wants their feeds checked,
// nil meaning "decide per feed", and reports how many feeds it brought forward.
//
// The schedule has to move with the preference, for the reason UpdateFeed's does:
// a reader who shortens their cadence and sees nothing happen for a day has been
// given a setting that appears not to work. Feeds with an override of their own are
// left alone — that is what an override is — and so are disabled ones, which a
// preference is not the place to revive.
//
// Nothing here can poll a feed sooner than the new cadence: the bound is measured
// from each feed's own last poll, so a list of seventy feeds settles into the
// cadence rather than emptying onto the worker at once. That is also why this needs
// no equivalent of PollNow's floor.
//
// One statement, so the preference and the schedule cannot disagree if the second
// half fails.
func (s *Store) SetDefaultPollInterval(ctx context.Context, userID UserID, d *time.Duration) (int64, error) {
	var moved int64
	err := s.pool.QueryRow(ctx, `
		WITH saved AS (
			UPDATE users SET default_poll_interval = make_interval(secs => $2::float8)
			WHERE id = $1
			RETURNING id
		), brought_forward AS (
			UPDATE feeds SET next_poll_at = COALESCE(last_polled_at, now())
			                                  + make_interval(secs => $2::float8)
			WHERE user_id = (SELECT id FROM saved)
			  AND poll_interval_override IS NULL
			  AND NOT disabled
			  -- Only feeds the new cadence brings forward. Without this, choosing a
			  -- longer cadence would push every feed out immediately, which is a
			  -- decision about feeds that are already due rather than about how often
			  -- they are checked from here on. A NULL cadence — back to automatic —
			  -- compares as NULL here and so moves nothing, which is right: there is
			  -- no cadence to bring anything forward to.
			  AND next_poll_at > COALESCE(last_polled_at, now())
			                       + make_interval(secs => $2::float8)
			RETURNING 1
		)
		SELECT count(*) FROM brought_forward`,
		userID, durationSeconds(d)).Scan(&moved)
	if err != nil {
		return 0, fmt.Errorf("storing the default poll interval for user %d: %w", userID, err)
	}
	return moved, nil
}

// SetMarkReadOnScroll stores whether unread lists mark articles read as they go
// past.
func (s *Store) SetMarkReadOnScroll(ctx context.Context, userID UserID, on bool) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET mark_read_on_scroll = $2 WHERE id = $1`, userID, on); err != nil {
		return fmt.Errorf("storing mark-read-on-scroll for user %d: %w", userID, err)
	}
	return nil
}
