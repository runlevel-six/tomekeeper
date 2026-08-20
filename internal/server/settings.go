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

	// PollEvery is the stored general cadence as the picker's value, PollChoices
	// are the options, and PollFloor names the shortest this instance will honor.
	PollEvery   string
	PollChoices []store.PollChoice
	PollFloor   string

	// Problem is a reason a save did not happen, in the reader's terms.
	Problem string

	// Export describes the download offered here, or is nil when the archive could
	// not be counted.
	Export *exportSummary
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, false, "")
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

	// Read before anything is written, so a cadence that means nothing costs the
	// save rather than leaving the palette stored and the interval not.
	interval, ok := store.PollIntervalFor(r.PostFormValue("poll_every"))
	if !ok {
		s.renderSettings(w, r, http.StatusBadRequest, false,
			"That is not a checking interval, so nothing was saved.")
		return
	}

	// Assembled from the known lists rather than taken from the form, so a
	// hand-crafted POST cannot put an arbitrary string into an HTML attribute.
	theme := store.ThemeValue(r.PostFormValue("palette"), r.PostFormValue("mode"))

	if err := s.store.SetTheme(r.Context(), userID, theme); err != nil {
		s.log.Error("saving the theme failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// The count is logged rather than shown. "How often" is the question the reader
	// asked; how many feeds that moved forward is the worker's business, and it is
	// the number worth having when somebody asks why a poll happened when it did.
	moved, err := s.store.SetDefaultPollInterval(r.Context(), userID, interval)
	if err != nil {
		s.log.Error("saving the default poll interval failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.log.Info("stored the default poll interval",
		"poll_every", store.PollChoiceValue(interval), "brought_forward", moved)

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

	s.renderSettings(w, r, http.StatusOK, true, "")
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int,
	saved bool, problem string,
) {
	page := settingsPage{
		pageData:  s.pageData(r, "settings"),
		Palettes:  store.Palettes,
		Modes:     store.Modes,
		Saved:     saved,
		Problem:   problem,
		Export:    s.exportSummaryFor(r),
		PollFloor: s.pollFloorLabel(),
	}
	page.Palette, page.Mode = store.SplitTheme(page.Theme)
	// Read back from pageData rather than from the form that may have just set it, so
	// the checkbox reports what is stored — a save that failed silently would
	// otherwise show as a save that worked. The cadence picker is read back for the
	// same reason.
	page.MarkOnScroll = page.MarkReadOnScroll
	page.PollEvery = store.PollChoiceValue(page.DefaultPollInterval)
	page.PollChoices = s.pollChoices(page.PollEvery)

	s.render(w, status, "settings", page)
}
