package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// Marking articles read as the reader scrolls past them.
//
// The one place in this interface where reading state changes without anybody
// pressing anything, which is why it is opt-in, why it is confined to the unread
// lists, and why the rules are written down here rather than left to the script.
//
// The division of labor: the page's script decides *when* an article has been gone
// past — a dwell on screen, then leaving upwards — and this decides *whether* it may
// be marked. Nothing the browser sends is taken as permission. The preference is
// re-read on every request, so a tab left open since before the reader turned the
// setting off stops having an effect at the moment they turn it off rather than
// whenever they next reload. And the exclusions live in the store, in
// MarkReadAutomatically, so they hold for any future caller that has the same
// business here.

// maxScrolledIDs bounds one request.
//
// A page is fifty rows and the script flushes long before filling one, so this is
// only ever reached by something other than the script — and the honest response to
// that is to refuse rather than to mark the first two hundred and drop the rest
// silently. Two hundred is four pages: generous for a real backlog of unflushed
// rows, small enough that one statement stays one statement.
const maxScrolledIDs = 200

// handleMarkScrolledRead marks a batch of scrolled-past articles read.
//
// A POST because it writes, and a batch because the alternative is a request per
// row: a reader clearing a morning's backlog would otherwise fire two hundred
// requests at their own server, each one a round trip that has to finish before the
// next row can be reported.
//
// The response is the affected rows' controls, as out-of-band fragments. That keeps
// one definition of those controls — the same partial a page renders and a click
// returns — rather than teaching the script how to redraw a button. Rows it did not
// mark are not in the response, so a row that was starred in another tab keeps the
// state it really has.
func (s *Server) handleMarkScrolledRead(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that request could not be read", http.StatusBadRequest)
		return
	}

	userID := signedInUser(r)

	prefs, err := s.store.GetPreferences(r.Context(), userID)
	if err != nil {
		s.log.Error("reading preferences failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !prefs.MarkReadOnScroll {
		// Not an error the reader caused, and not a 404 either: the route exists and
		// the request was well formed. Refusing here is what makes turning the
		// preference off take effect on pages that were already open.
		http.Error(w, "marking read on scroll is off", http.StatusConflict)
		return
	}

	ids, ok := parseArticleIDs(r.PostFormValue("ids"))
	if !ok {
		http.Error(w, "that list of articles could not be read", http.StatusBadRequest)
		return
	}
	if len(ids) == 0 {
		// Nothing to do, and nothing to redraw. A flush that raced a reload can
		// legitimately arrive empty.
		s.renderFragmentList(w, http.StatusOK, "actions", nil)
		return
	}
	if len(ids) > maxScrolledIDs {
		http.Error(w, "too many articles in one request", http.StatusBadRequest)
		return
	}

	marked, err := s.store.MarkReadAutomatically(r.Context(), userID, ids)
	if err != nil {
		s.log.Error("marking scrolled articles read failed", "count", len(ids), "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Debug rather than info: this happens continuously while somebody reads, and a
	// log line per batch would bury everything else. The count is what would matter
	// if this were ever suspected of marking more than it should.
	s.log.Debug("marked scrolled articles read", "asked", len(ids), "marked", len(marked))

	items := make([]any, 0, len(marked))
	for _, m := range marked {
		// Starred is false by construction — the query cannot mark a starred article
		// — and Kept comes back from the write, so every field of the shared partial
		// is filled from what the database now holds rather than from what this
		// request happened to change.
		items = append(items, actions{
			ArticleID: m.ArticleID,
			Read:      true,
			Starred:   false,
			Kept:      m.Kept,
			OOB:       true,
		})
	}
	s.renderFragmentList(w, http.StatusOK, "actions", items)
}

// parseArticleIDs reads the comma-separated ids one flush carries.
//
// All or nothing: a malformed list is refused rather than partly applied, because
// the only thing that sends one is a script, and a script sending junk is a bug
// whose symptom should not be "some of your articles were marked read".
func parseArticleIDs(raw string) ([]store.ArticleID, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}

	fields := strings.Split(raw, ",")
	ids := make([]store.ArticleID, 0, len(fields))
	seen := make(map[store.ArticleID]struct{}, len(fields))

	for _, field := range fields {
		n, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil || n <= 0 {
			return nil, false
		}
		id := store.ArticleID(n)
		// Deduplicated because a row can be scrolled past twice before a flush — up
		// and back down again — and the cap should count articles rather than
		// repetitions. Not a correctness fix: `= ANY(array)` yields one row per
		// article however many times its id appears.
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, true
}
