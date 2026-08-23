package server

import (
	"net/http"

	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/jobs"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// reprocessPage re-extracts a reader's whole archive from the pages already stored.
//
// The last operator-only per-reader capability. Per-domain reprocessing has been on
// the rules page since the rules page existed, but the whole-archive form was
// `tome reextract` and nothing else — a terminal, and on Kubernetes permission to
// exec into a pod. That was defensible while extraction was the household's: bringing
// one archive-wide body set forward is maintenance. Once a reader owns their own
// bodies it is theirs to redo, and asking somebody else to run a command against a
// database is not a way to own anything.
//
// **It re-extracts; it never fetches.** Every input is already on disk, which is the
// standing payoff for keeping the raw page: an extraction improvement or a rule can
// be applied to a decade of archive, including sites that no longer exist, without
// asking a single server for anything.
type reprocessPage struct {
	pageData

	// Version is what this build extracts at, so "out of date" has a stated meaning
	// rather than being a claim the page makes about itself.
	Version string

	// Mine is every mutable body this reader holds of their own, and MineStale is how
	// many of those came from a different extractor version.
	//
	// Both, because they answer different questions and a page offering one number
	// would answer the wrong one half the time: after an upgrade the reader wants the
	// stale ones, and after a spell of editing rules they want the lot.
	Mine      int
	MineStale int

	// Household is the same pair for the bodies every reader sees unless their own
	// extraction diverges. Counted only for an administrator, who is the only person
	// who may move them.
	Household      int
	HouseholdStale int

	// CanHousehold says whether this reader may reprocess what everybody reads.
	//
	// Administrators only, and shown rather than implied — "did I just re-extract my
	// own copies or everybody's" is the question this page must never leave
	// ambiguous.
	CanHousehold bool

	// Available says whether work can actually be queued, which needs a job queue.
	// Without one the page says so instead of offering a button that does nothing.
	Available bool

	// Outcome reports what a submission did.
	Outcome *reprocessOutcome
}

// reprocessOutcome is what the page says after queueing.
type reprocessOutcome struct {
	Problem string

	// Queued is how many articles were enqueued, and Whose and StaleOnly say what
	// that number was a count of. Reported together on purpose: "queued 40 articles"
	// reads identically whether it moved one reader's private copies or the bodies
	// the whole household reads, and those are very different things to have just
	// done.
	Queued   int
	Whose    string
	Everyone bool

	StaleOnly bool
}

// reprocessScopeStale selects only bodies from another extractor version;
// reprocessScopeAll selects every mutable body in the slot.
//
// The second is expressed to the store as "bring everything to version 0", because
// the selection is "any version other than this one" and no extraction has ever
// recorded 0. Named rather than inlined for the same reason reprocessEveryVersion is:
// a bare "0" at a call site reads like a count.
const (
	reprocessScopeStale = "stale"
	reprocessScopeAll   = "all"
)

// reprocessCount is one dry-run walk: where the number goes, whose bodies it counts,
// and which version it compares against.
type reprocessCount struct {
	into    *int
	owner   *store.UserID
	version string
}

func (s *Server) handleReprocessConfirm(w http.ResponseWriter, r *http.Request) {
	s.renderReprocess(w, r, http.StatusOK, nil)
}

// handleReprocess queues the work.
//
// Whose bodies move follows the form, and the household is refused to anybody who is
// not an administrator rather than silently downgraded to their own. A control that
// quietly did something smaller than it said would leave a reader believing the
// archive had been reprocessed when only their own copies had.
func (s *Server) handleReprocess(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	account := signedInAccount(r)
	everyone := r.PostFormValue("whose") == "household"
	if everyone && !account.IsAdmin() {
		s.renderReprocess(w, r, http.StatusForbidden, &reprocessOutcome{
			Problem: "Only an administrator can re-extract the bodies everybody reads. " +
				"Nothing was queued.",
		})
		return
	}

	staleOnly := r.PostFormValue("scope") != reprocessScopeAll

	if s.jobs == nil {
		// The command that does the same thing, named exactly, because an instance
		// with no queue is an instance whose owner is already on a terminal.
		s.renderReprocess(w, r, http.StatusOK, &reprocessOutcome{
			Problem: "This instance cannot queue re-extraction, because it has no job " +
				"queue configured. `tome reextract` does the same thing.",
		})
		return
	}

	owner := store.Owned(account.ID)
	whose := "your own bodies"
	if everyone {
		owner = store.Household()
		whose = "the bodies everybody reads"
	}

	version := extract.Version
	if !staleOnly {
		version = reprocessEveryVersion
	}

	// Scoped by construction rather than by a check above it: candidates come from
	// one owner's slot, so a reader's run cannot reach another reader's bodies even
	// if this handler were wrong about who is asking.
	queued, err := jobs.QueueReextraction(r.Context(), s.store, s.jobs, jobs.ReextractRequest{
		Version: version,
		Owner:   owner,
	})
	if err != nil {
		s.log.Error("queueing a whole-archive re-extraction failed",
			"user_id", account.ID, "household", everyone, "error", err)
		s.renderReprocess(w, r, http.StatusInternalServerError, &reprocessOutcome{
			Problem: "Re-extraction could not be queued. The log will say why. Anything " +
				"already queued is safe to leave; re-running this is harmless.",
		})
		return
	}

	s.log.Info("queued a whole-archive re-extraction",
		"user_id", account.ID, "household", everyone, "stale_only", staleOnly, "articles", queued)

	s.renderReprocess(w, r, http.StatusOK, &reprocessOutcome{
		Queued:    queued,
		Whose:     whose,
		Everyone:  everyone,
		StaleOnly: staleOnly,
	})
}

func (s *Server) renderReprocess(w http.ResponseWriter, r *http.Request, status int,
	outcome *reprocessOutcome,
) {
	account := signedInAccount(r)

	page := reprocessPage{
		pageData:     s.pageData(r, "settings"),
		Version:      extract.Version,
		CanHousehold: account.IsAdmin(),
		// A queue is needed to *queue*, not to count: the counts below are dry runs,
		// so an instance without one can still say what a reprocess would move.
		Available: s.jobs != nil,
		Outcome:   outcome,
	}

	counts := []reprocessCount{
		{&page.Mine, store.Owned(account.ID), reprocessEveryVersion},
		{&page.MineStale, store.Owned(account.ID), extract.Version},
	}
	// The household's pair is counted only for somebody who could act on it. Two
	// dry-run walks of the whole archive is a real cost, and a number nobody may use
	// is not worth one.
	if page.CanHousehold {
		counts = append(counts,
			reprocessCount{&page.Household, store.Household(), reprocessEveryVersion},
			reprocessCount{&page.HouseholdStale, store.Household(), extract.Version})
	}

	for _, c := range counts {
		// A dry run of the same walk the button performs, which is what makes the
		// number on the page and the work it queues the same selection rather than two
		// that agree until one of them changes.
		n, err := jobs.QueueReextraction(r.Context(), s.store, s.jobs, jobs.ReextractRequest{
			Version: c.version,
			Owner:   c.owner,
			DryRun:  true,
		})
		if err != nil {
			s.log.Error("counting what a reprocess would move failed",
				"user_id", account.ID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		*c.into = n
	}

	s.render(w, status, "reprocess", page)
}
