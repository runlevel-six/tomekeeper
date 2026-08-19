package server

import (
	"net/http"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// Removing a subscription — the one thing the feeds page could not do, which turned
// an ordinary correction into a database session.
//
// It is the last step of a chain the interface already had: a feed that has moved is
// edited, and a feed the reader has two of has to lose one. Until this existed, the
// refusal that protects against two subscriptions to one address was a dead end,
// because the only way out of it was `DELETE FROM feeds`.
//
// Two steps, like marking a list read, and for the same reason: it cannot be undone
// one button at a time. Unlike that control it also has to say what it costs, because
// the answer is not always "nothing" — see store.FeedRemoval.

// unsubscribeAsk builds the confirmation for one subscription.
//
// Nil for an id that is not a live feed of this reader's, which leaves the page as it
// was: the same fallback ?edit= takes, and the right answer for a link followed twice.
func (s *Server) unsubscribeAsk(r *http.Request, raw string) *unsubscribeControl {
	f := s.feedByRawID(r, raw)
	if f == nil {
		return nil
	}

	ask := &unsubscribeControl{Feed: *f, Confirming: true}

	removal, err := s.store.FeedRemoval(r.Context(), signedInUser(r), f.ID)
	if err != nil {
		// The question is still worth asking without the numbers, but not silently:
		// the count is the part that says whether this is free, so a missing one has
		// to read as unknown rather than as zero.
		s.log.Warn("measuring a feed removal failed", "feed_id", int64(f.ID), "error", err)
		ask.Problem = "How much this subscription carries could not be counted, so what " +
			"unsubscribing costs is unknown. The log will say why."
		return ask
	}
	ask.Removal = removal

	return ask
}

// handleUnsubscribeFeed removes one subscription.
//
// The feed is read first so the page that reports the removal can still name what it
// removed, and so a feed belonging to somebody else is a 404 before anything is
// deleted rather than a delete that quietly matches no rows.
func (s *Server) handleUnsubscribeFeed(w http.ResponseWriter, r *http.Request) {
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

	existing, err := s.store.GetFeed(r.Context(), userID, feedID)
	if err != nil {
		s.notFoundOrError(w, r, err, "looking up a feed to unsubscribe from")
		return
	}

	// Counted before the delete, because afterwards there is nothing left to count and
	// the reader is owed the number they were shown.
	removal, err := s.store.FeedRemoval(r.Context(), userID, feedID)
	if err != nil {
		// Not fatal: the removal is what was asked for, and a missing count costs the
		// sentence that describes it.
		s.log.Warn("measuring a feed removal failed", "feed_id", id, "error", err)
	}

	removed, err := s.store.DeleteFeed(r.Context(), userID, feedID)
	if err != nil {
		s.log.Error("unsubscribing from a feed failed", "feed_id", id, "error", err)
		s.renderFeedsWith(w, r, http.StatusInternalServerError, feedsExtras{
			Unsubscribe: &unsubscribeControl{
				Feed: existing, Done: true,
				Problem: "That subscription could not be removed. The log will say why.",
			},
		})
		return
	}
	if !removed {
		// Gone between the read and the delete — another tab. Not an error.
		http.NotFound(w, r)
		return
	}

	s.log.Info("unsubscribed from a feed",
		"feed_id", id, "url", existing.FeedURL,
		"articles", removal.Articles, "stranded", removal.Stranded)

	s.renderFeedsWith(w, r, http.StatusOK, feedsExtras{
		Unsubscribe: &unsubscribeControl{Feed: existing, Removal: removal, Done: true},
	})
}
