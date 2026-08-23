package server

import (
	"net/http"

	"github.com/runlevel-six/tomekeeper/internal/jobs"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// handleRefetch asks the origin for a page this archive already has.
//
// The one remedy for a problem the stored copy cannot be talked out of. Extraction
// runs over stored bytes, so when the bytes themselves are wrong — a page whose
// images sit behind URLs that have since expired, or one that needed a browser
// before anybody flagged the domain — re-extracting cannot help and only the origin
// can.
//
// A POST, because it costs the origin a request: a GET would be followed by every
// crawler and link prefetcher that saw the page. The politeness rules still apply
// underneath — robots.txt and the per-host rate limit are the fetcher's business, and
// a domain flagged as needing a browser is handed to one, which is what makes this
// the fix for the second case.
func (s *Server) handleRefetch(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}

	// Scoped before anything is queued: this spends somebody else's bandwidth, so it
	// must not be reachable for an article the reader cannot see.
	if _, err := s.store.ArticleForUser(r.Context(), signedInUser(r), store.ArticleID(id)); err != nil {
		s.notFoundOrError(w, r, err, "reading an article to fetch again")
		return
	}

	if s.jobs == nil {
		// The server runs without a queue in some deployments; saying so is better
		// than a button that silently does nothing.
		s.log.Warn("asked to fetch a page again with no job queue configured")
		http.Error(w, "this instance has no worker to fetch with", http.StatusServiceUnavailable)
		return
	}

	if err := jobs.EnqueueRefetch(r.Context(), s.jobs, store.ArticleID(id)); err != nil {
		s.log.Error("queueing a re-fetch failed", "article_id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.log.Info("queued a re-fetch", "article_id", id)
	http.Redirect(w, r, refetchReturn(r.PostFormValue("from")), http.StatusSeeOther)
}

// refetchReturn maps the form's `from` to where the reader goes next.
//
// A fixed set of destinations rather than the submitted value, which is the whole
// point: a redirect built from what a form posted is an open redirect, and this one
// would be reachable by anybody who can get a reader to submit a form. The field was
// posted and ignored until now — every button landed on the attention queue, which
// is wrong from the audit page, where a reader who fixes four titles in a row was
// thrown off the page after the first.
func refetchReturn(from string) string {
	switch from {
	case "audit":
		return "/attention/audit"
	default:
		return "/attention"
	}
}
