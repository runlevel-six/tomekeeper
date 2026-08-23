package server

import (
	"net/http"

	"github.com/runlevel-six/tomekeeper/internal/explain"
	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// explainPage says why an article's body looks the way it does.
type explainPage struct {
	pageData

	Report explain.Report

	// Version is what this build extracts at, so "stale" has a stated meaning.
	Version string

	// Unavailable says the archive directory could not be opened, in which case
	// there is nothing to run the ladder over and the page says so rather than
	// reporting an empty explanation as an answer.
	Unavailable bool
}

// handleExplain runs the extraction ladder over one article's stored page and
// reports what each rung produced.
//
// The command this replaces needed a terminal and, on Kubernetes, permission to
// exec into a pod. Once a reader can write their own rules, the person who most
// needs to know why a selector produced nothing is the one least likely to have
// either — so the same function backs both, and neither can drift from the other.
//
// It runs the ladder rather than reading a stored explanation, which is why it
// needs the archive directory. Nothing is written and no request leaves the
// machine: every input is already on disk.
func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	articleID := store.ArticleID(id)
	userID := signedInUser(r)

	// Visibility first and separately, so that an article this reader may not see
	// is not found rather than explained. The explanation would otherwise describe
	// a page they are not entitled to know exists.
	if _, err := s.store.ArticleForUser(r.Context(), userID, articleID); err != nil {
		s.notFoundOrError(w, r, err, "reading an article before explaining it")
		return
	}

	page := explainPage{pageData: s.pageData(r, "attention"), Version: extract.Version}

	if s.blobs == nil {
		page.Unavailable = true
		s.render(w, http.StatusOK, "explain", page)
		return
	}

	report, err := explain.For(r.Context(), s.store, s.blobs, userID, articleID)
	if err != nil {
		s.notFoundOrError(w, r, err, "explaining an article")
		return
	}
	page.Report = report

	s.render(w, http.StatusOK, "explain", page)
}
