package jobs

import (
	"context"
	"fmt"

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

	// Owner is whose bodies to bring forward — nil for the household's, which is
	// what a bare reextract means and what it has always done.
	//
	// A reader's run reaches only their own forks. It deliberately does not create
	// forks for articles they have none of: a reader without one reads the
	// household's body, and giving them a private copy of every article in the
	// archive is the opposite of what copy-on-write is for.
	Owner *store.UserID

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
		candidates, err := s.System().ReextractCandidates(
			ctx, req.Owner, req.Version, req.Domain, cursor, reextractBatch)
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
				if err := EnqueueExtraction(ctx, client, c.ArticleID, req.Owner, true); err != nil {
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

// ruleExtractionBatch bounds one enqueueing pass when a rule is written.
//
// The largest single host in a real archive here is under three hundred articles,
// so this is a ceiling rather than a paging loop — and if a host ever exceeds it,
// the backstop sweep enqueues the remainder on its next run rather than this
// blocking a form submission for as long as the host is large.
const ruleExtractionBatch = 1000

// QueueRuleExtraction enqueues extraction for the articles a newly written rule
// applies to, for the owner who wrote it.
//
// The eager half of "a rule change reprocesses its articles". It runs inside the
// request that saved the rule, so the reader is told how many articles are being
// reconsidered rather than left wondering whether anything happened.
//
// It is not the guarantee. The worker may be down at this moment — on this
// deployment the server and the worker are separate Deployments, so that is every
// rollout — in which case nothing here is queued and the backstop sweep picks the
// work up instead. That is why this returns a count and not a promise.
func QueueRuleExtraction(
	ctx context.Context,
	s *store.Store,
	client *river.Client[pgx.Tx],
	owner store.UserID,
	domain string,
) (int, error) {
	var slot *store.UserID
	if owner != store.HouseholdRule {
		slot = store.Owned(owner)
	}
	articles, err := s.System().ArticlesUnderRule(ctx, slot, domain, ruleExtractionBatch)
	if err != nil {
		return 0, err
	}
	if len(articles) == 0 {
		return 0, nil
	}

	params := make([]river.InsertManyParams, 0, len(articles))
	for _, id := range articles {
		params = append(params, river.InsertManyParams{
			Args: ExtractArticleArgs{
				ArticleID: int64(id),
				UserID:    int64(owner),
				// Force, because the point of writing a rule is that the body it
				// would now produce differs from the one on record. The job's own
				// skip check compares the ruleset and would reach the same
				// conclusion; saying so here means a rule saved twice in a row does
				// the work twice rather than appearing to do nothing the second
				// time, which is what somebody adjusting a selector is doing.
				Force: true,
			},
		})
	}

	results, err := client.InsertMany(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("enqueueing extraction for %s: %w", domain, err)
	}

	var inserted int
	for _, r := range results {
		if !r.UniqueSkippedAsDuplicate {
			inserted++
		}
	}
	return inserted, nil
}
