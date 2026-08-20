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

	// MarkOnScroll is the stored preference, which is what the checkbox shows.
	MarkOnScroll bool

	// Export describes the download offered here, or is nil when the archive could
	// not be counted.
	Export *exportSummary
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

	userID := signedInUser(r)

	// Assembled from the known lists rather than taken from the form, so a
	// hand-crafted POST cannot put an arbitrary string into an HTML attribute.
	theme := store.ThemeValue(r.PostFormValue("palette"), r.PostFormValue("mode"))

	if err := s.store.SetTheme(r.Context(), userID, theme); err != nil {
		s.log.Error("saving the theme failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// An absent checkbox means off — which is only safe because this form always
	// posts every preference it holds. A partial POST would silently turn things off,
	// so a future preference that arrives from somewhere else needs its own route
	// rather than a second reading of this one.
	if err := s.store.SetMarkReadOnScroll(r.Context(), userID,
		r.PostFormValue("mark_on_scroll") == "on"); err != nil {
		s.log.Error("saving the mark-on-scroll preference failed", "error", err)
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
		Export:   s.exportSummaryFor(r),
	}
	page.Palette, page.Mode = store.SplitTheme(page.Theme)
	// Read back from pageData rather than from the form that may have just set it, so
	// the checkbox reports what is stored — a save that failed silently would
	// otherwise show as a save that worked.
	page.MarkOnScroll = page.MarkReadOnScroll

	s.render(w, status, "settings", page)
}
