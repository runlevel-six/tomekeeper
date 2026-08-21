package store

import (
	"context"
	"fmt"
)

// The named type-size steps, smallest first.
//
// Names rather than numbers because the content security policy has no
// 'unsafe-inline' for styles: the value cannot ride to the browser as a style
// attribute, so it travels as a data attribute the stylesheet maps — and a
// stylesheet maps names. What each name is worth is a ratio in tome.css, which is
// the only place it is written down.
const (
	TextScaleSmaller = "smaller"
	TextScaleNormal  = "normal"
	TextScaleLarger  = "larger"
	TextScaleLargest = "largest"
)

// TextScales are the steps a reader may choose, in the order the settings page
// offers them.
func TextScales() []string {
	return []string{TextScaleSmaller, TextScaleNormal, TextScaleLarger, TextScaleLargest}
}

// TextScaleValue normalizes whatever arrived in a form.
//
// Anything unrecognized becomes 'normal' rather than an error, for the reason the
// palette does the same: a settings form is not a place where a reader should be
// able to produce a failure, and a value this does not know is a value the
// stylesheet cannot map anyway — it would render as normal whatever were stored.
// Storing what will actually be shown keeps the page honest about its own state.
func TextScaleValue(v string) string {
	for _, s := range TextScales() {
		if v == s {
			return v
		}
	}
	return TextScaleNormal
}

// SetTextScale stores one reader's chosen type size.
func (s *Store) SetTextScale(ctx context.Context, userID UserID, scale string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET text_scale = $2 WHERE id = $1`, userID, TextScaleValue(scale)); err != nil {
		return fmt.Errorf("storing the text scale for user %d: %w", userID, err)
	}
	return nil
}
