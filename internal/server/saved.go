package server

import (
	"errors"
	"net/http"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// savedPage is the reading list: pages saved by hand, and the box that saves one.
type savedPage struct {
	streamPage

	// Saved reports on a save that just happened, if one did.
	Saved *savedOutcome
}

type savedOutcome struct {
	URL       string
	Problem   string
	ArticleID store.ArticleID
	Already   bool
	HasBody   bool
}

func (s *Server) handleSaved(w http.ResponseWriter, r *http.Request) {
	s.renderSaved(w, r, http.StatusOK, nil)
}

// handleSave archives a URL the reader pasted in.
//
// The Wallabag half of the archive: a page nothing subscribed to, kept because
// someone decided to keep it. It deduplicates against everything already here,
// so saving a link a feed happened to carry costs nothing and is readable at
// once.
//
// No job is enqueued. A new article is left at fetch_status 'pending', and the
// scheduler the worker already runs sweeps those up — so the web process never
// has to be a job producer. The reader is told the fetch is queued rather than
// done, because within a scheduler interval it will not be.
func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	userID := signedInUser(r)

	if err := r.ParseForm(); err != nil {
		s.renderSaved(w, r, http.StatusBadRequest, &savedOutcome{
			Problem: "That form did not arrive intact. Try again.",
		})
		return
	}

	raw := r.PostFormValue("url")
	if raw == "" {
		s.renderSaved(w, r, http.StatusBadRequest, &savedOutcome{
			Problem: "No address was given.",
		})
		return
	}

	saved, err := s.store.SaveArticle(r.Context(), userID, raw)
	switch {
	case errors.Is(err, store.ErrNotSaveable):
		s.renderSaved(w, r, http.StatusBadRequest, &savedOutcome{
			URL:     raw,
			Problem: "That is not a web address this can archive.",
		})
		return
	case err != nil:
		s.log.Error("saving a page failed", "error", err)
		s.renderSaved(w, r, http.StatusInternalServerError, &savedOutcome{
			URL:     raw,
			Problem: "Something went wrong saving that. It has not been archived.",
		})
		return
	}

	s.log.Info("saved a page",
		"article_id", saved.ArticleID, "already_saved", saved.AlreadySaved, "has_body", saved.HasBody)

	s.renderSaved(w, r, http.StatusOK, &savedOutcome{
		URL:       saved.Canonical,
		ArticleID: saved.ArticleID,
		Already:   saved.AlreadySaved,
		HasBody:   saved.HasBody,
	})
}

// renderSaved draws the reading list, optionally reporting on a save.
//
// Like the OPML import, this renders rather than redirects so the result cannot
// be lost, and re-posting is harmless because saving is idempotent.
func (s *Server) renderSaved(w http.ResponseWriter, r *http.Request, status int, saved *savedOutcome) {
	userID := signedInUser(r)

	q := store.StreamQuery{SavedOnly: true, Limit: store.DefaultStreamLimit + 1}
	if before := r.URL.Query().Get("before"); before != "" {
		sortAt, id, ok := parseCursor(before)
		if !ok {
			http.Error(w, "that page marker could not be read", http.StatusBadRequest)
			return
		}
		q.BeforeSort, q.BeforeID = sortAt, id
	}

	items, err := s.store.Stream(r.Context(), userID, q)
	if err != nil {
		s.log.Error("listing saved pages failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	page := savedPage{
		streamPage: streamPage{
			pageData: s.pageData(r, "saved"),
			Heading:  "Saved",
			Empty:    "Nothing saved yet. Paste a link above to archive a page.",
		},
		Saved: saved,
	}
	if len(items) > store.DefaultStreamLimit {
		last := items[store.DefaultStreamLimit-1]
		items = items[:store.DefaultStreamLimit]
		page.NextPage = "/saved?before=" + formatCursor(last.SortAt, last.ArticleID)
	}
	page.Items = items

	if isHTMX(r) && r.URL.Query().Get("before") != "" {
		s.renderFragment(w, http.StatusOK, "stream-rows", page.streamPage)
		return
	}
	s.render(w, status, "saved", page)
}
