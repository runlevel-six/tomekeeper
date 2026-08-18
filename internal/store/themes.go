package store

import (
	"context"
	"fmt"
)

// Theme is a palette choice, stored as the value the page sets on <html
// data-theme>.
type Theme struct {
	// Value is what goes in the attribute, and what is stored. Empty for auto.
	Value string

	// Palette and Mode are the two independent halves of the choice, kept apart
	// so the settings page can offer them as two questions rather than one list
	// of nineteen.
	Palette string
	Mode    string

	Name  string
	Blurb string

	// LightBG and DarkBG are the palette's page background in each mode.
	//
	// Duplicated from the stylesheet, which is a real cost, and paid because
	// mobile browsers color their own chrome from a <meta name="theme-color">
	// and cannot read a CSS variable to do it. A test asserts each value still
	// appears in the stylesheet for its palette, so the copy cannot drift
	// silently.
	LightBG string
	DarkBG  string
}

// Palettes are the six from the design sheet, plus the original neutral one.
//
// The order is the order they appear in the picker, and "auto" is first because
// it is what everyone has until they choose otherwise.
var Palettes = []Theme{
	{Palette: "auto", Name: "Default", Blurb: "The original neutral palette.", LightBG: "#fbfaf7", DarkBG: "#16151a"},
	{Palette: "midnight", Name: "Midnight", Blurb: "Deep navy and gold leaf.", LightBG: "#f2e8d5", DarkBG: "#0d1d34"},
	{Palette: "plum", Name: "Royal Plum", Blurb: "Aubergine and antique gold.", LightBG: "#f1e7d8", DarkBG: "#341e3e"},
	{Palette: "verdant", Name: "Verdant Archive", Blurb: "Bottle green on aged parchment.", LightBG: "#e7d4ae", DarkBG: "#1e4032"},
	{Palette: "oxblood", Name: "Oxblood", Blurb: "Oxblood leather and copper.", LightBG: "#efe2cf", DarkBG: "#4e1b21"},
	{Palette: "slate", Name: "Slate & Silver", Blurb: "Graphite and silver, no gold.", LightBG: "#eee7da", DarkBG: "#2e323a"},
	{Palette: "aegean", Name: "Aegean Bronze", Blurb: "Deep teal and warm bronze.", LightBG: "#eee4d1", DarkBG: "#0b4151"},
}

// Modes are how a palette decides between its light and dark halves.
var Modes = []struct{ Value, Name string }{
	{"", "Follow the system"},
	{"light", "Always light"},
	{"dark", "Always dark"},
}

// ThemeValue builds the attribute value for a palette and mode, or "auto".
//
// Validated by construction rather than by parsing: the only values that can
// ever reach the database are ones assembled here from the two lists above, so
// a stored theme cannot name a palette the stylesheet does not define.
func ThemeValue(palette, mode string) string {
	if palette == "" {
		palette = "auto"
	}

	known := false
	for _, p := range Palettes {
		if p.Palette == palette {
			known = true
			break
		}
	}
	if !known {
		return "auto"
	}

	switch mode {
	case "light", "dark":
		return palette + "-" + mode
	default:
		return palette
	}
}

// SplitTheme recovers the palette and mode from a stored value.
func SplitTheme(value string) (palette, mode string) {
	if value == "" || value == "auto" {
		return "auto", ""
	}
	for _, suffix := range []string{"-light", "-dark"} {
		if len(value) > len(suffix) && value[len(value)-len(suffix):] == suffix {
			return value[:len(value)-len(suffix)], suffix[1:]
		}
	}
	return value, ""
}

// GetTheme returns the reader's stored palette.
func (s *Store) GetTheme(ctx context.Context, userID UserID) (string, error) {
	var theme string
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(theme, 'auto') FROM users WHERE id = $1`, userID).Scan(&theme); err != nil {
		return "auto", fmt.Errorf("reading the theme for user %d: %w", userID, err)
	}
	return theme, nil
}

// SetTheme stores the reader's palette.
func (s *Store) SetTheme(ctx context.Context, userID UserID, theme string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET theme = $2 WHERE id = $1`, userID, theme); err != nil {
		return fmt.Errorf("storing the theme for user %d: %w", userID, err)
	}
	return nil
}
