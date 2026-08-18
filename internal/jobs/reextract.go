package jobs

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// ReextractRequest selects what to re-extract.
type ReextractRequest struct {
	// Version is the extractor version everything should end up at. Articles whose
	// body came from any *other* version are the candidates — the predicate is "not
	// this one", never "since this one".
	Version string

	// Domain, when set, restricts the work to one host and its subdomains. The
	// common reason to reprocess is a rule just written for one badly-extracting
	// site, and without this, fixing one site means re-extracting everything.
	Domain string

	// Limit stops after queueing this many. Zero means no limit.
	Limit int

	// DryRun counts without queueing.
	DryRun bool

	// Progress, when set, is called with a running total every few hundred
	// articles, so a long reprocess can say something before it finishes.
	Progress func(queued int)
}

// reextractBatch is how many candidates are fetched per round trip.
const reextractBatch = 500

// reextractProgressEvery is how often Progress is called.
const reextractProgressEvery = 2000

// QueueReextraction walks the candidates and enqueues each one, returning how many.
//
// Shared by `tome reextract` and by the reprocess control in the web interface, so
// that the two cannot disagree about what "reprocess this domain" means. That
// mattered enough to move here from the command: the button exists because
// reprocessing is the second half of writing a domain rule, and a button that
// selected a different set of articles than the documented command would be worse
// than no button.
//
// The client is insert-only in both callers. This queues work for the worker pool
// and runs none of it, so re-extracting an entire archive is subject to the same
// concurrency limits as everything else, and neither a CLI invocation nor a web
// request holds anything open while it happens.
func QueueReextraction(
	ctx context.Context,
	s *store.Store,
	client *river.Client[pgx.Tx],
	req ReextractRequest,
) (int, error) {
	var (
		total  int
		cursor store.ArticleID
	)

	for {
		// Candidates are selected by the store with `NOT immutable` in the WHERE
		// clause rather than filtered here. That is deliberate: imported bodies must
		// be *provably* skipped, and a WHERE clause is a proof while a conditional
		// in a loop is a promise.
		candidates, err := s.System().ReextractCandidates(ctx, req.Version, req.Domain, cursor, reextractBatch)
		if err != nil {
			return total, err
		}
		if len(candidates) == 0 {
			return total, nil
		}

		for _, c := range candidates {
			cursor = c.ArticleID

			if req.Limit > 0 && total >= req.Limit {
				return total, nil
			}
			if !req.DryRun {
				// Forced: these articles all have a current body already, and without
				// it the worker would see one and skip.
				if err := EnqueueExtraction(ctx, client, c.ArticleID, true); err != nil {
					return total, err
				}
			}
			total++

			if req.Progress != nil && total%reextractProgressEvery == 0 {
				req.Progress(total)
			}
		}
	}
}
