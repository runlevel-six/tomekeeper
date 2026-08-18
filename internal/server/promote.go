package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// handlePromoteBody makes one of an article's stored bodies the one shown.
//
// The deliberate human act the archive's rules leave room for, and the only one
// there is. An imported body is immutable and wins over everything automatically,
// because it may be the only surviving copy of a page that is gone — so nothing
// automatic may replace it, and an article imported with a thin copy of a page this
// archive has since fetched properly would otherwise be stuck with the thin one
// forever. Somebody looks at both and decides; this is that.
//
// **The choice is global, like the body it chooses.** Bodies are a property of the
// article, and the archive keeps one copy of a page for everyone — so promoting one
// changes what every reader sees, in a way starring or tagging never does. Correct
// while there is one reader; a multi-user build has to decide whether this stays a
// shared decision or becomes a per-reader preference, and that is a question about
// what an archive is rather than a permission to add.
func (s *Server) handlePromoteBody(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	articleID := store.ArticleID(id)

	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	bodyID, err := strconv.ParseInt(r.PostFormValue("body"), 10, 64)
	if err != nil || bodyID <= 0 {
		http.NotFound(w, r)
		return
	}

	// The reader must be able to see the article before they can change what it
	// shows. Bodies carry no user scoping of their own — they belong to the article
	// — so this is where that check has to happen, and it is the only thing standing
	// between a hand-crafted form and somebody else's archive.
	if _, err := s.store.ArticleForUser(r.Context(), signedInUser(r), articleID); err != nil {
		s.notFoundOrError(w, r, err, "reading an article before promoting a body")
		return
	}

	if err := s.store.PromoteBody(r.Context(), articleID, store.ContentID(bodyID)); err != nil {
		if errors.Is(err, store.ErrNoSuchBody) {
			// The body is not this article's. Not found rather than a complaint: the
			// same answer as an article that does not exist, for the same reason.
			http.NotFound(w, r)
			return
		}
		s.log.Error("promoting a body failed",
			"article_id", articleID, "body_id", bodyID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.log.Info("promoted a stored body", "article_id", articleID, "body_id", bodyID)

	// Rendered rather than redirected, like the other forms here: the reader is
	// looking at the article and the result is the article, now showing something
	// else. Saying so matters — two bodies of the same page can look similar enough
	// that a silent swap reads as nothing having happened.
	s.serveArticle(w, r, articleID, "This is now the copy shown for this article.")
}
