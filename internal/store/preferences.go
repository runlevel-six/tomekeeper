package store

import (
	"context"
	"fmt"
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
}

// GetPreferences returns one reader's settings.
//
// A failure returns usable defaults alongside the error, so a caller that would
// rather draw the page than fail it — which is every caller — does not have to
// invent them. The default palette is 'auto' and automatic marking is off, which
// is what a reader who has never opened the settings page has.
func (s *Store) GetPreferences(ctx context.Context, userID UserID) (Preferences, error) {
	prefs := Preferences{Theme: "auto"}

	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(theme, 'auto'), mark_read_on_scroll
		FROM users WHERE id = $1`, userID,
	).Scan(&prefs.Theme, &prefs.MarkReadOnScroll); err != nil {
		return Preferences{Theme: "auto"}, fmt.Errorf("reading preferences for user %d: %w", userID, err)
	}
	return prefs, nil
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
