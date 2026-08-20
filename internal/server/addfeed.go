package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/runlevel-six/tomekeeper/internal/feed"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// addFeedOutcome is the state of the one-subscription form: testing an address,
// adding it, and editing a subscription that already exists.
//
// One type for all three, because they are steps of one task and the page has to
// show the state of that task: what was typed, what testing found, and what saving
// did. Splitting them would mean a form that forgets what the reader entered the
// moment anything goes wrong, which is the failure that makes people stop using a
// form.
type addFeedOutcome struct {
	// URL, Category and Title are what was submitted, echoed back so the form is
	// still filled in.
	URL      string
	Category string
	Title    string

	// Enabled is Feed.Disabled inverted, and only the edit form draws it. Inverted
	// because "check this feed" is the sentence somebody reads on a checkbox,
	// whereas an unchecked box meaning "not disabled" is the one they misread.
	Enabled bool

	// PollEvery is the cadence picker's value — empty for automatic — and, like
	// Enabled, is drawn only by the edit form. Held as the posted string rather than
	// as a duration so that a value the store refuses comes back selected: the
	// reader should see what they chose next to the reason it was not kept.
	PollEvery string

	// EditingID is the subscription this form has open, and zero on the form that
	// adds a new one. It survives a test — a hidden field carries it through —
	// because otherwise checking a corrected address would lose which subscription
	// was being corrected, and Save would create a second one.
	EditingID store.FeedID

	// FeedTitle is what the edited subscription is called, for the heading over the
	// form and for the line confirming a save. It outlives EditingID by one render
	// for exactly that reason: a saved edit closes the form and still has to say
	// what it saved.
	FeedTitle string

	// Problem is a reason the step could not be completed, in the reader's terms.
	Problem string

	// Probed is what a test found, and is nil unless one succeeded.
	Probed *feed.Probed

	// Added and Existing report a subscription that was saved.
	Added    bool
	Existing bool

	// Saved reports an edit that was applied, and URLChanged, Reenabled and
	// TurnedOff say what it did to polling — which is the half of an edit that
	// leaves no trace in the row it changed.
	Saved      bool
	URLChanged bool
	Reenabled  bool
	TurnedOff  bool

	// PollChanged reports a cadence that is not what it was, and Cadence is the new
	// one in words. Reported for the same reason as the three above: the feed list
	// has no column for it, so a save that changed nothing else would look like a
	// save that did nothing.
	PollChanged bool
	Cadence     string

	// AlreadySubscribed is set when a test found a feed this reader already has.
	// Not an error: it is the answer to "am I subscribed to this?", which is a
	// reasonable thing to use a test for.
	AlreadySubscribed bool
}

// submittedForm reads the fields the form posts, whichever button was pressed.
func submittedForm(r *http.Request) *addFeedOutcome {
	return &addFeedOutcome{
		URL:      strings.TrimSpace(r.PostFormValue("url")),
		Category: strings.TrimSpace(r.PostFormValue("category")),
		Title:    strings.TrimSpace(r.PostFormValue("title")),
		// Absent on the add form, which has no such control: a new subscription is
		// polled, and a checkbox offering to create one that is not would be a
		// choice nobody wants to make at that moment.
		Enabled: r.PostFormValue("enabled") != "",
		// Also absent on the add form, and for a similar reason: how often to check
		// a feed nobody has fetched yet is a question with no information behind it.
		// The reader's general preference already covers it, and the row can be given
		// a cadence of its own once it exists.
		PollEvery: strings.TrimSpace(r.PostFormValue("poll_every")),
	}
}

// handleTestFeed fetches a feed URL and reports what is there, saving nothing.
//
// This is the one place `tome serve` makes an outbound request, and it is worth
// being explicit about why the rule it bends is still intact. The rule is that
// polling belongs to the worker, because seventy origin servers thinking about it
// must not be able to make the reader unresponsive. This is one request, to one
// host, that a reader explicitly asked for and is waiting on — the same shape as a
// reader clicking a link, not the shape of a scheduler. If no HTTP client is
// configured the control says so rather than failing obscurely.
func (s *Server) handleTestFeed(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	outcome := submittedForm(r)

	// The form may have a subscription open rather than be adding one. Testing must
	// come back as the edit form it was, so the id travels in a hidden field and is
	// re-checked against the reader's own feeds here — it arrives from a request like
	// anything else.
	if raw := strings.TrimSpace(r.PostFormValue("edit")); raw != "" {
		if f := s.feedByRawID(r, raw); f != nil {
			outcome.EditingID = f.ID
			outcome.FeedTitle = f.Title
		}
	}

	if outcome.URL == "" {
		outcome.Problem = "No address was given."
		s.renderFeedsWith(w, r, http.StatusBadRequest, feedsExtras{Add: outcome})
		return
	}

	if s.fetch == nil {
		outcome.Problem = "This instance cannot test a feed, because it has no outbound HTTP client " +
			"configured. Adding it without testing still works: the worker will report what it finds."
		s.renderFeedsWith(w, r, http.StatusOK, feedsExtras{Add: outcome})
		return
	}

	probed, err := feed.Probe(r.Context(), s.fetch, outcome.URL)
	if err != nil {
		outcome.Problem = testFailureMessage(err)
		s.log.Info("testing a feed failed", "url", outcome.URL, "error", err)
		s.renderFeedsWith(w, r, http.StatusOK, feedsExtras{Add: outcome})
		return
	}

	outcome.Probed = &probed

	// The URL to subscribe to is whatever answered, which may be the feed a site
	// advertised rather than the address that was typed. Putting it back in the
	// form is what makes the second step correct without the reader retyping it.
	outcome.URL = probed.FeedURL
	if outcome.Title == "" {
		outcome.Title = probed.Title
	}

	// Already subscribed is an answer, not a failure. Except when the subscription it
	// finds is the one being edited, where it is no answer at all — "you are already
	// subscribed to this" about the feed whose address you are correcting is a
	// sentence that stops somebody mid-edit for no reason.
	if existing, err := s.store.FeedByURL(r.Context(), signedInUser(r), probed.FeedURL); err == nil {
		outcome.AlreadySubscribed = existing.ID != outcome.EditingID
		if outcome.Category == "" {
			outcome.Category = existing.Category
		}
	} else if !store.IsNotFound(err) {
		s.log.Warn("checking for an existing subscription failed", "error", err)
	}

	s.log.Info("tested a feed",
		"url", probed.FeedURL, "items", probed.Items, "discovered", probed.Discovered)

	s.renderFeedsWith(w, r, http.StatusOK, feedsExtras{Add: outcome})
}

// handleAddFeed subscribes to one feed.
//
// No fetch here, deliberately. Testing is a separate step the reader may skip —
// somebody pasting a feed URL they have used for years should not be made to wait
// for a round trip — and a feed that turns out to be broken is exactly what the
// feed list's health column and the attention queue are for.
func (s *Server) handleAddFeed(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	outcome := submittedForm(r)

	normalized, err := feed.NormalizeFeedURL(outcome.URL)
	if err != nil {
		outcome.Problem = "That is not a web address. A feed URL usually ends in something like " +
			"/feed, /rss or /atom.xml."
		s.renderFeedsWith(w, r, http.StatusBadRequest, feedsExtras{Add: outcome})
		return
	}
	outcome.URL = normalized

	feedID, created, err := s.store.UpsertFeed(r.Context(), signedInUser(r), store.FeedParams{
		FeedURL:  normalized,
		Title:    outcome.Title,
		Category: outcome.Category,
	})
	if err != nil {
		s.log.Error("adding a feed failed", "url", normalized, "error", err)
		outcome.Problem = "That subscription could not be saved. The log will say why."
		s.renderFeedsWith(w, r, http.StatusInternalServerError, feedsExtras{Add: outcome})
		return
	}

	outcome.Added = created
	outcome.Existing = !created

	s.log.Info("added a feed", "url", normalized, "feed_id", int64(feedID), "created", created)

	// The form is cleared on success. What a reader wants next is the feed they
	// just added visible in the list below, not their own typing still in the box.
	if created {
		outcome.URL, outcome.Title, outcome.Category = "", "", ""
	}

	s.renderFeedsWith(w, r, http.StatusOK, feedsExtras{Add: outcome})
}

// handleEditFeed changes one subscription: its address, its title, the folder it is
// filed under, and whether it is polled at all.
//
// Every one of those was an UPDATE somebody had to write by hand until now, and the
// documented cure for a feed that had moved was to subscribe again at the new
// address and abandon the row — which throws away the poll history that says how
// long it had been broken, and leaves a dead subscription in the list. Correcting
// the row keeps both.
//
// Nothing here fetches, for the same reason adding does not: whether the new address
// answers is a separate question, and Test is the button that asks it.
func (s *Server) handleEditFeed(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	userID := signedInUser(r)
	feedID := store.FeedID(id)

	// Read before writing, for two reasons: a feed that is not this reader's is a 404
	// before anything is changed, and the confirmation has to be able to say what
	// changed, which means knowing what the row held.
	existing, err := s.store.GetFeed(r.Context(), userID, feedID)
	if err != nil {
		s.notFoundOrError(w, r, err, "looking up a feed to edit")
		return
	}

	outcome := submittedForm(r)
	outcome.EditingID = feedID
	// The name the feed had when the form was opened, not whatever is half-typed in
	// the title box — the heading over a form should not change as somebody types
	// into it.
	outcome.FeedTitle = existing.Title

	normalized, err := feed.NormalizeFeedURL(outcome.URL)
	if err != nil {
		outcome.Problem = "That is not a web address. A feed URL usually ends in something like " +
			"/feed, /rss or /atom.xml."
		s.renderFeedsWith(w, r, http.StatusBadRequest, feedsExtras{Add: outcome})
		return
	}
	outcome.URL = normalized

	// Refused rather than rounded or ignored. The only way to get here is a
	// hand-written POST or a picker this release no longer offers, and in both cases
	// storing something other than what was asked for — including "automatic" — is
	// worse than saying no.
	interval, ok := store.PollIntervalFor(outcome.PollEvery)
	if !ok {
		outcome.Problem = "That is not a checking interval, so nothing was changed."
		s.renderFeedsWith(w, r, http.StatusBadRequest, feedsExtras{Add: outcome})
		return
	}

	updated, err := s.store.UpdateFeed(r.Context(), userID, feedID, store.FeedEdit{
		FeedURL:      normalized,
		Title:        outcome.Title,
		Category:     outcome.Category,
		Disabled:     !outcome.Enabled,
		PollInterval: interval,
	})
	switch {
	case errors.Is(err, store.ErrFeedURLTaken):
		// Naming the other subscription is the whole difference between a refusal
		// somebody can act on and one that reads as the form having lost track of
		// which feed it had open. The usual cause is a feed listed twice by an OPML
		// import — the old address and the new one — where the row being edited is
		// the one that never worked, and what the reader wants next is to remove it.
		outcome.Problem = collisionMessage(s.otherFeedAt(r, normalized, feedID))
		s.renderFeedsWith(w, r, http.StatusConflict, feedsExtras{Add: outcome})
		return
	case store.IsNotFound(err):
		// Between the read above and this write, so somebody removed the feed in
		// another tab. Rare, and still not a 500.
		http.NotFound(w, r)
		return
	case err != nil:
		s.log.Error("editing a feed failed", "feed_id", id, "url", normalized, "error", err)
		outcome.Problem = "That subscription could not be saved. The log will say why."
		s.renderFeedsWith(w, r, http.StatusInternalServerError, feedsExtras{Add: outcome})
		return
	}

	s.log.Info("edited a feed", "feed_id", id, "url", updated.FeedURL,
		"url_changed", updated.FeedURL != existing.FeedURL,
		"category", updated.Category, "disabled", updated.Disabled,
		"poll_every", store.PollChoiceValue(updated.PollIntervalOverride))

	// The form closes and goes back to being the add form: what a reader wants to
	// look at after an edit is the row they just corrected, which is in the list
	// below. The confirmation carries what the edit did to polling, because that part
	// of it is invisible in the row.
	s.renderFeedsWith(w, r, http.StatusOK, feedsExtras{Add: &addFeedOutcome{
		Saved:      true,
		FeedTitle:  updated.Title,
		URLChanged: updated.FeedURL != existing.FeedURL,
		Reenabled:  existing.Disabled && !updated.Disabled,
		TurnedOff:  updated.Disabled,
		PollChanged: store.PollChoiceValue(existing.PollIntervalOverride) !=
			store.PollChoiceValue(updated.PollIntervalOverride),
		Cadence: cadencePhrase(updated),
	}})
}

// otherFeedAt is the reader's other subscription to an address, if the lookup
// succeeds. Nil means "there is one, but this cannot say which" — the constraint has
// already proved it exists.
func (s *Server) otherFeedAt(r *http.Request, feedURL string, editing store.FeedID) *store.Feed {
	other, err := s.store.FeedByURL(r.Context(), signedInUser(r), feedURL)
	if err != nil || other.ID == editing {
		if err != nil && !store.IsNotFound(err) {
			s.log.Warn("looking up the colliding subscription failed", "url", feedURL, "error", err)
		}
		return nil
	}
	return &other
}

// collisionMessage explains a refused address in terms of the subscription holding it.
//
// The sentence has to do two things: say which feed, and say what to do about it.
// Without the first it reads as the form having lost track of which subscription was
// open; without the second it is a dead end, because the way out — removing one of the
// two — is a control the reader has to be told exists.
func collisionMessage(other *store.Feed) string {
	if other == nil {
		return "Another of your subscriptions already uses that address, so nothing was " +
			"changed. Two subscriptions to one feed would be indistinguishable in this list."
	}

	msg := "“" + other.Title + "” already uses that address, so nothing was changed — two " +
		"subscriptions to one feed would be indistinguishable in this list. "
	switch {
	case other.LastSuccessAt != nil:
		// The common case: an OPML import carried both the old address and the new
		// one, and the working row is the other one.
		msg += "That one is already being polled successfully, so this subscription is the " +
			"spare — Unsubscribe below removes it and keeps everything it archived."
	default:
		msg += "That one has never fetched successfully either, so the address may be wrong " +
			"in both. Test it before saving, or unsubscribe from whichever of the two you " +
			"do not want."
	}
	return msg
}

// feedByRawID resolves a feed id that arrived as text, or nil.
//
// Nil rather than an error: every caller is filling in a form, and the honest
// response to a stale or malformed id is the form without it — an error page about
// the id in a hidden field tells the reader nothing they can act on.
func (s *Server) feedByRawID(r *http.Request, raw string) *store.Feed {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	f, err := s.store.GetFeed(r.Context(), signedInUser(r), store.FeedID(id))
	if err != nil {
		s.log.Info("no such feed to edit", "feed_id", id, "error", err)
		return nil
	}
	return &f
}

// editForm loads one subscription into the form at the top of the feeds page.
func (s *Server) editForm(r *http.Request, raw string) *addFeedOutcome {
	f := s.feedByRawID(r, raw)
	if f == nil {
		return nil
	}
	return &addFeedOutcome{
		EditingID: f.ID,
		FeedTitle: f.Title,
		URL:       f.FeedURL,
		Title:     f.Title,
		Category:  f.Category,
		Enabled:   !f.Disabled,
		// This feed's own cadence, not the one in force: the picker sets the
		// override, and showing the reader's general preference selected here would
		// turn opening the form and saving it into a way to pin every feed to
		// whatever the preference happened to be that day.
		PollEvery: store.PollChoiceValue(f.PollIntervalOverride),
	}
}

// testFailureMessage turns a probe failure into something worth reading.
//
// The distinction that matters to somebody standing at the form is between "that
// address is not a feed" and "that address did not answer", because the first is
// theirs to fix and the second may fix itself. The underlying error goes to the
// log; this is the sentence on the page.
func testFailureMessage(err error) string {
	switch {
	case errors.Is(err, feed.ErrNoFeedDiscovered):
		return "That address is a web page rather than a feed, and it does not advertise one. " +
			"Look for a feed link on the site, or try adding /feed or /rss to the address."
	case errors.Is(err, feed.ErrNotAFeed):
		return "That address answered, but with something this cannot read as a feed."
	default:
		return "That address could not be fetched: " + err.Error()
	}
}
