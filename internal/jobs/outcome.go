package jobs

import (
	"context"
	"errors"
	"time"
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
