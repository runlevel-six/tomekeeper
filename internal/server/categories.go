package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// handleCreateCategory adds an empty folder.
//
// Empty is the point: it is what free text could not express, so "make a folder,
// then move feeds into it" was a sequence a reader could not perform.
func (s *Server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("name")
	_, err := s.store.CreateCategory(r.Context(), signedInUser(r), name)
	switch {
	case errors.Is(err, store.ErrCategoryNameBlank):
		s.renderCategories(w, r, http.StatusBadRequest, "", "A category needs a name.")
		return
	case errors.Is(err, store.ErrCategoryExists):
		// Named, because the useful thing to say is which name is taken — and it is
		// one they can see in the list below.
		s.renderCategories(w, r, http.StatusConflict, "",
			"You already have a category called "+name+".")
		return
	case err != nil:
		s.log.Error("creating a category failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.renderCategories(w, r, http.StatusOK, "Added "+name+".", "")
}

// handleRenameCategory changes a folder's name and nothing else.
//
// The operation that used to be a rewrite of every feed in the folder, and used to
// break Fever clients: the group id was derived from the name, so a rename made the
// old folder vanish from a client and a new one appear holding the same feeds. The
// id belongs to the row now.
func (s *Server) handleRenameCategory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	id, ok := categoryIDFrom(r.PostFormValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	name := r.PostFormValue("name")
	err := s.store.RenameCategory(r.Context(), signedInUser(r), id, name)
	switch {
	case errors.Is(err, store.ErrCategoryNameBlank):
		s.renderCategories(w, r, http.StatusBadRequest, "", "A category needs a name.")
		return
	case errors.Is(err, store.ErrCategoryExists):
		s.renderCategories(w, r, http.StatusConflict, "",
			"You already have a category called "+name+".")
		return
	case errors.Is(err, pgx.ErrNoRows):
		// Not found, never forbidden: a distinct refusal would confirm that somebody
		// else's category exists.
		http.NotFound(w, r)
		return
	case err != nil:
		s.log.Error("renaming a category failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.renderCategories(w, r, http.StatusOK, "Renamed to "+name+".", "")
}

// handleDeleteCategory removes a folder and disposes of its feeds.
//
// **No branch of this touches an article.** Nothing in this project deletes one, and
// an article has no category of its own to lose — it is derived through feed_items to
// the feed that carried it, so refiling a feed moves everything it ever brought in.
// The destructive reading of "delete this folder" is served by unsubscribing, which
// already exists, already asks first, and keeps every article it archived.
func (s *Server) handleDeleteCategory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	id, ok := categoryIDFrom(r.PostFormValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	how := store.CategoryDisposition(r.PostFormValue("disposition"))
	var into store.CategoryID
	if how == store.DispositionMove {
		into, ok = categoryIDFrom(r.PostFormValue("into"))
		if !ok {
			s.renderCategories(w, r, http.StatusBadRequest, "",
				"Choose a category to move those feeds into.")
			return
		}
	}

	result, err := s.store.DeleteCategory(r.Context(), signedInUser(r), id, how, into)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		http.NotFound(w, r)
		return
	case err != nil:
		s.log.Error("deleting a category failed", "error", err, "disposition", how)
		s.renderCategories(w, r, http.StatusInternalServerError, "",
			"Nothing was changed. The log will say why.")
		return
	}

	s.log.Info("deleted a category", "disposition", how,
		"feeds", result.Feeds, "unsubscribed", result.Unsubscribed)
	s.renderCategories(w, r, http.StatusOK, describeDisposition(how, result), "")
}

// describeDisposition says what a deletion did, in the terms the reader chose it in.
//
// It names the feeds and says nothing about articles, because nothing happened to
// any: a message mentioning them would imply otherwise.
func describeDisposition(how store.CategoryDisposition, r store.DeleteCategoryResult) string {
	switch how {
	case store.DispositionUnsubscribe:
		if r.Unsubscribed == 0 {
			return "Deleted the category. It had no feeds in it."
		}
		// "Nothing was deleted" is true and, on its own, misleading. Unsubscribing
		// removes the feed reference, and an article is visible when a feed points at
		// it *or* the reader has acted on it — so anything they never opened, starred
		// or saved stops being listed while remaining on disk. That is the same
		// consequence unsubscribing one feed has always had, and a reader who is told
		// only the reassuring half will conclude the interface lost their articles.
		return "Deleted the category and unsubscribed " +
			plural(int(r.Unsubscribed), "feed") +
			". Nothing they archived was deleted, though anything you never opened or " +
			"saved is no longer listed."
	case store.DispositionMove:
		if r.Feeds == 0 {
			return "Deleted the category. It had no feeds to move."
		}
		return "Deleted the category and moved " + plural(int(r.Feeds), "feed") + "."
	default:
		if r.Feeds == 0 {
			return "Deleted the category. It had no feeds in it."
		}
		return "Deleted the category. " + plural(int(r.Feeds), "feed") + " now filed under nothing."
	}
}

func categoryIDFrom(raw string) (store.CategoryID, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return store.CategoryID(id), true
}
