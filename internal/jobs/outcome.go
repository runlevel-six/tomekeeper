package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// Recording how a job ended is a separate concern from doing the work, because
// the outcome most worth writing down is the one where the work ran out of time.
//
// River gives every job a context with a deadline — one minute, unless the
// client says otherwise, which this one does not — and cancels that context when
// the worker shuts down. Both are right for the work and wrong for the write
// that reports it: work which consumed the whole minute leaves behind a context
// that can no longer carry a query, so the article's failure cannot be recorded
// at the one moment it most needs to be.
//
// Measured on the live archive before this existed: one article whose host had
// stopped answering spent four days looping. The fetch used the full minute,
// RecordFetchFailure was handed the expired context, its UPDATE could not run,
// and the job returned "context deadline exceeded" — so River retried it, the
// fetch ran out of time again, and the article stayed 'pending' with fetch_error
// NULL. That combination is invisible to the attention queue, which lists
// failures and pending-with-a-reason; and ScheduleFetchesWorker enqueues every
// 'pending' article it finds, so the loop outlived the job's 25 attempts and
// would have run for as long as the archive did.
//
// An article nobody can see is an article nobody can fix, which is what makes
// failing to record worse than what there was to record.

// recordTimeout bounds a write that reports how a job ended.
//
// Short deliberately. The work is over by this point and all that is left is one
// UPDATE against a primary key, on a pool the rest of the worker is still
// sharing — so a hung write here should give up rather than hold a connection
// for as long as the job itself was allowed.
const recordTimeout = 10 * time.Second

// recording returns a context for writing down how a job ended, detached from
// the job's own deadline and cancellation.
func recording(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
}

// interrupted reports whether the worker is shutting down, as opposed to the job
// having run out of its own time.
//
// The distinction decides whether an outcome may be recorded at all. Running out
// of time is the page's failure and belongs in the attention queue; a shutdown is
// ours, and River hands a job interrupted by one to the next worker that starts.
// Recording that as the article's failure would permanently fail whatever was in
// flight during every rolling restart, because a recorded failure is never
// retried — so the archive would lose articles in proportion to how often it was
// deployed.
//
// Tested as context.Canceled rather than as "not DeadlineExceeded" so that a
// fetch canceled for some third reason keeps the benefit of the doubt too.
func interrupted(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

// describe renders an error as the reader of the attention queue will see it,
// given what to say when a deadline is the whole story.
//
// "context deadline exceeded" names a Go value. The queue is read by somebody
// deciding whether a host needs a domain rule, and what they need to know is
// that the page did not arrive in the time allowed.
//
// The error is what gets tested, not the context, because either deadline
// reaching first means the same thing: the client's own timeout bounds one
// request, the job's bounds everything the job did. Whichever fired, nothing
// arrived.
func describe(err error, outOfTime string) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return outOfTime
	}
	return err.Error()
}

// recordFetchFailure writes down that no page arrived, then gives the feed's own
// body a chance to stand in for it.
//
// Recording the failure and stopping there was the behavior for the whole life of
// the fetch worker, and it threw away content the archive already had. The
// extraction ladder's fourth rung exists for exactly this — viaFeedBody runs when
// RawHTML is empty, and readRaw returns nil for an article with no stored page on
// purpose — but nothing ever enqueued an extraction for a fetch that failed, so
// the rung could not be reached from the pipeline. Measured on a real archive:
// nine articles sat bodyless on a feed body that would have extracted, seven of
// them from a site whose full-text feed the reader pays for.
//
// It matters most for the failures that are permanent. A 403 or a robots refusal
// is recorded rather than retried, and neither StaleBodies nor ArticlesUnderRule
// will look at an article with no raw_blob_path — so without this the feed's copy
// is unreachable by every route except a re-fetch that succeeds.
//
// Applied to a robots refusal as well as to an outright failure. The feed body was
// not crawled from the page: it arrived in a feed the reader subscribed to, which
// is the same reasoning that exempts feed polls from robots.txt in the first place.
// A site that asked not to have its pages crawled has not asked for its own feed to
// go unread.
func recordFetchFailure(
	ctx context.Context,
	s *store.Store,
	id store.ArticleID,
	status, reason string,
	log *slog.Logger,
) error {
	recCtx, cancel := recording(ctx)
	defer cancel()

	if err := s.RecordFetchFailure(recCtx, id, status, reason); err != nil {
		return err
	}

	salvageFromFeed(recCtx, s, id, log)
	return nil
}

// salvageFromFeed enqueues extraction for an article whose page never arrived,
// when the feed it came from carried a body worth extracting.
//
// Nothing here is fatal, and that is the deliberate part. The fetch failure is
// already written down by the time this runs, and returning an error would hand
// the job back to River — which would re-run the fetch, asking the origin again
// for a page it has just refused. The whole design of this worker is that a
// recorded failure is never retried, and salvaging a body is not a good enough
// reason to break it.
//
// The cost of giving up is honest and worth stating: no sweep will find this
// article later, so its feed body waits until somebody runs `tome refetch`, which
// runs this path again. A warning in the log is what makes that visible.
//
// Whether the body clears the ladder's floor is not decided here. This asks only
// whether there is anything to try; viaFeedBody applies the 200-character
// threshold, and duplicating it would be a second copy of a rule that already
// exists.
func salvageFromFeed(ctx context.Context, s *store.Store, id store.ArticleID, log *slog.Logger) {
	body, err := s.FeedBodyFor(ctx, id)
	if err != nil {
		log.Warn("could not look for a feed body to fall back on", "error", err)
		return
	}
	if body == "" {
		return
	}

	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		log.Warn("no river client in context; the feed body will not be extracted", "error", err)
		return
	}

	// Not Force: an article that already has a body has one worth more than this,
	// and ExtractArticleWorker's own currency check is what decides that.
	if _, err := client.Insert(ctx, ExtractArticleArgs{ArticleID: int64(id)}, nil); err != nil {
		log.Warn("could not enqueue extraction of the feed body", "error", err)
		return
	}

	log.Info("no page arrived, so the feed's own body will be extracted instead",
		"feed_body_chars", len(body))
}
