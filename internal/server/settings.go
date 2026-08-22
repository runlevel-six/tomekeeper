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

	// TextSizes are the named steps, and TextSize is the stored one.
	TextSizes []struct{ Value, Name, Blurb string }
	TextSize  string

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

	// ConfirmSignOut asks before ending every session.
	//
	// A two-step confirmation rather than a button that acts, like the bulk mark
	// and unsubscribe: the content security policy has no unsafe-inline, so there
	// is no JavaScript dialog available, and this is an action whose effect is
	// mostly on devices the reader is not looking at.
	ConfirmSignOut bool
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, false, "")
}

// handleSignOutEverywhere revokes every session this reader has, including the one
// making the request.
//
// Bumping the epoch is what does it; clearing the cookie afterwards is a courtesy
// so this browser goes straight to the sign-in page instead of presenting a
// credential that will now be refused.
func (s *Server) handleSignOutEverywhere(w http.ResponseWriter, r *http.Request) {
	userID := signedInUser(r)

	if _, err := s.store.System().BumpSessionEpoch(r.Context(), userID); err != nil {
		s.log.Error("signing out everywhere failed", "user_id", userID, "error", err)
		s.renderSettings(w, r, http.StatusInternalServerError, false,
			"Those sessions could not be ended. The log will say why, and nothing changed.")
		return
	}

	s.log.Info("signed out everywhere", "user_id", userID)
	s.sessions.Clear(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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

	// Normalized against the known steps for the same reason: this value is written
	// into a data attribute on the document element, and the stylesheet can only
	// map the names it knows.
	if err := s.store.SetTextScale(r.Context(), userID,
		store.TextScaleValue(r.PostFormValue("text_size"))); err != nil {
		s.log.Error("saving the text size failed", "error", err)
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

		ConfirmSignOut: r.URL.Query().Get("signout") == "all",
	}
	page.Palette, page.Mode = store.SplitTheme(page.Theme)

	// Named steps with a word about each, because "Larger" alone does not say
	// larger than what. The blurbs are the only thing that makes the difference
	// between two adjacent steps legible before choosing one.
	page.TextSizes = []struct{ Value, Name, Blurb string }{
		{store.TextScaleSmaller, "Smaller", "More on screen at once"},
		{store.TextScaleNormal, "Normal", "What this has always been"},
		{store.TextScaleLarger, "Larger", "Easier at arm's length"},
		{store.TextScaleLargest, "Largest", "Largest this goes"},
	}
	page.TextSize = page.TextScale
	// Read back from pageData rather than from the form that may have just set it, so
	// the checkbox reports what is stored — a save that failed silently would
	// otherwise show as a save that worked. The cadence picker is read back for the
	// same reason.
	page.MarkOnScroll = page.MarkReadOnScroll
	page.PollEvery = store.PollChoiceValue(page.DefaultPollInterval)
	page.PollChoices = s.pollChoices(page.PollEvery)

	s.render(w, status, "settings", page)
}
