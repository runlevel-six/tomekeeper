package server

import (
	"errors"
	"net/http"

	"github.com/runlevel-six/tomekeeper/internal/feed"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// maxOPMLUpload bounds an uploaded subscription list.
//
// A 500-feed export is on the order of 100KB, so 4MB is roughly two orders of
// magnitude of headroom and still small enough that a mistake — a video dropped
// on the wrong form — is rejected before it is read rather than after it has been
// buffered. The limit is applied to the request body, not to the parsed form:
// ParseMultipartForm's argument only decides what stays in memory, and the
// remainder spills to disk, so on its own it bounds nothing.
const maxOPMLUpload = 4 << 20

// maxOPMLMemory is how much of that may stay in memory before spilling to a
// temporary file. Below the whole limit deliberately, so the memory cost of an
// upload is bounded independently of how large an upload is allowed to be.
const maxOPMLMemory = 1 << 20

// handleImportOPML takes an uploaded OPML file and subscribes to everything in it.
//
// This renders the feeds page directly rather than redirecting to it, so the
// result is attached to the request that produced it and cannot be lost to a
// redirect the reader's browser followed before they read anything. Re-posting is
// harmless — imports are keyed by feed URL and idempotent — which is what makes
// the missing redirect an acceptable trade rather than a bug.
//
// There is no CSRF token, for the same reason the rest of the mutating routes
// have none: the session cookie is SameSite=Lax, so a cross-site POST does not
// carry it and arrives unauthenticated. That is a property of the cookie, so it
// holds for a multipart upload exactly as it does for a form button.
func (s *Server) handleImportOPML(w http.ResponseWriter, r *http.Request) {
	userID := signedInUser(r)

	// Before ParseMultipartForm, so an oversized body is refused as it arrives
	// instead of being spooled to disk first.
	r.Body = http.MaxBytesReader(w, r.Body, maxOPMLUpload)

	if err := r.ParseMultipartForm(maxOPMLMemory); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.renderFeedsWith(w, r, http.StatusRequestEntityTooLarge, feedsExtras{
				Imported: &importOutcome{
					Problem: "That file is larger than 4MB, which is far larger than any subscription list. " +
						"Check that it is the OPML export and not something else.",
				},
			})
			return
		}
		s.log.Warn("reading an OPML upload failed", "error", err)
		s.renderFeedsWith(w, r, http.StatusBadRequest, feedsExtras{
			Imported: &importOutcome{Problem: "The upload did not arrive intact. Try again."},
		})
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	file, header, err := r.FormFile("opml")
	if err != nil {
		s.renderFeedsWith(w, r, http.StatusBadRequest, feedsExtras{
			Imported: &importOutcome{Problem: "No file was chosen."},
		})
		return
	}
	defer func() { _ = file.Close() }()

	// Deliberately not gated on Content-Type. Browsers report OPML as any of
	// text/xml, application/xml, text/x-opml or application/octet-stream
	// depending on the platform's file associations, so a check would reject
	// valid files on some machines and not others. The parser is the honest
	// arbiter of whether this is OPML.
	subs, err := feed.ParseOPML(file)
	if err != nil {
		s.log.Info("an uploaded OPML file could not be parsed", "filename", header.Filename, "error", err)
		s.renderFeedsWith(w, r, http.StatusBadRequest, feedsExtras{
			Imported: &importOutcome{
				Filename: header.Filename,
				Problem: "That does not look like an OPML file. Most readers call the export " +
					"“subscriptions”, “OPML” or “feeds”, and the file usually ends in .opml or .xml.",
			},
		})
		return
	}
	if len(subs) == 0 {
		s.renderFeedsWith(w, r, http.StatusBadRequest, feedsExtras{
			Imported: &importOutcome{
				Filename: header.Filename,
				Problem:  "That file parsed as OPML but contains no subscriptions.",
			},
		})
		return
	}

	result := feed.Import(r.Context(), s.store, userID, subs)

	// Failures are logged as well as shown. The page reports them to the reader
	// who is standing there; the log is what remains afterwards.
	for _, f := range result.Failures {
		s.log.Warn("a subscription in an uploaded OPML file could not be stored",
			"feed_url", f.FeedURL, "error", f.Err)
	}

	s.log.Info("imported subscriptions from an upload",
		"filename", header.Filename,
		"added", result.Added, "existing", result.Existing, "failed", len(result.Failures))

	s.renderFeedsWith(w, r, http.StatusOK, feedsExtras{
		Imported: &importOutcome{Filename: header.Filename, Result: &result},
	})
}

// importOutcome is what the feeds page says about an import that just happened.
//
// Either a Problem — the file never got as far as the database — or a Result.
type importOutcome struct {
	Filename string
	Problem  string
	Result   *feed.ImportResult
}

// handleRefreshFeeds brings every one of the reader's feeds forward to due.
//
// Note what this does not do: it does not fetch anything. `tome serve` has no job
// client and no business holding a request open while seventy origin servers think
// about it — polling is the worker's work. So this moves the schedule and says so,
// which is the honest thing to put behind a button labeled "check now": the
// alternative is a spinner that finishes before any feed has been contacted.
//
// Rendered rather than redirected, like the OPML import above and for the same
// reason: the result belongs to the request that produced it. Re-posting costs
// nothing, because the store leaves alone anything polled inside its floor.
func (s *Server) handleRefreshFeeds(w http.ResponseWriter, r *http.Request) {
	result, err := s.store.PollNow(r.Context(), signedInUser(r))
	if err != nil {
		s.log.Error("bringing feed polls forward failed", "error", err)
		s.renderFeedsWith(w, r, http.StatusInternalServerError, feedsExtras{
			Refreshed: &refreshOutcome{Problem: "The feeds could not be queued. The log will say why."},
		})
		return
	}

	s.log.Info("feeds queued for an on-demand poll",
		"moved", result.Moved, "held", result.Held, "disabled", result.Disabled)

	s.renderFeedsWith(w, r, http.StatusOK, feedsExtras{Refreshed: &refreshOutcome{Result: &result}})
}

// refreshOutcome is what the feeds page says after a manual refresh.
type refreshOutcome struct {
	Problem string
	Result  *store.PollNowResult
}
