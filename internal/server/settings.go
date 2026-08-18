package server

import (
	"net/http"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// settingsPage is the reader's own preferences.
type settingsPage struct {
	pageData

	Palettes []store.Theme
	Modes    []struct{ Value, Name string }

	Palette string
	Mode    string
	Saved   bool
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, false)
}

// handleSaveSettings stores a changed preference.
//
// Renders rather than redirects, like the other forms here, so the confirmation
// belongs to the request that earned it. Re-posting is harmless: setting a theme
// to what it already is changes nothing.
func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	// Assembled from the known lists rather than taken from the form, so a
	// hand-crafted POST cannot put an arbitrary string into an HTML attribute.
	theme := store.ThemeValue(r.PostFormValue("palette"), r.PostFormValue("mode"))

	if err := s.store.SetTheme(r.Context(), signedInUser(r), theme); err != nil {
		s.log.Error("saving the theme failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.renderSettings(w, r, http.StatusOK, true)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, saved bool) {
	page := settingsPage{
		pageData: s.pageData(r, "settings"),
		Palettes: store.Palettes,
		Modes:    store.Modes,
		Saved:    saved,
	}
	page.Palette, page.Mode = store.SplitTheme(page.Theme)

	s.render(w, status, "settings", page)
}
