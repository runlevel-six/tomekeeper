package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/runlevel-six/tomekeeper/internal/feed"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// addFeedOutcome is what the feeds page says after a test or an add.
//
// One type for both, because they are two steps of one task and the page has to
// show the state of that task: what was typed, what testing found, and what
// subscribing did. Splitting them would mean a form that forgets what the reader
// entered the moment anything goes wrong, which is the failure that makes people
// stop using a form.
type addFeedOutcome struct {
	// URL, Category and Title are what was submitted, echoed back so the form is
	// still filled in.
	URL      string
	Category string
	Title    string

	// Problem is a reason the step could not be completed, in the reader's terms.
	Problem string

	// Probed is what a test found, and is nil unless one succeeded.
	Probed *feed.Probed

	// Added and Existing report a subscription that was saved.
	Added    bool
	Existing bool

	// AlreadySubscribed is set when a test found a feed this reader already has.
	// Not an error: it is the answer to "am I subscribed to this?", which is a
	// reasonable thing to use a test for.
	AlreadySubscribed bool
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

	outcome := &addFeedOutcome{
		URL:      strings.TrimSpace(r.PostFormValue("url")),
		Category: strings.TrimSpace(r.PostFormValue("category")),
		Title:    strings.TrimSpace(r.PostFormValue("title")),
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

	// Already subscribed is an answer, not a failure.
	if existing, err := s.store.FeedByURL(r.Context(), signedInUser(r), probed.FeedURL); err == nil {
		outcome.AlreadySubscribed = true
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

	outcome := &addFeedOutcome{
		URL:      strings.TrimSpace(r.PostFormValue("url")),
		Category: strings.TrimSpace(r.PostFormValue("category")),
		Title:    strings.TrimSpace(r.PostFormValue("title")),
	}

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
