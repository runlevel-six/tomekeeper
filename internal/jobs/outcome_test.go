package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

func TestDescribeSaysWhatRanOutOfTime(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{{
		// Wrapped, because this never arrives bare: the client returns a
		// *url.Error and ReadBody wraps again with %w.
		name: "a wrapped deadline reads as words",
		err:  fmt.Errorf("Get %q: %w", "https://example.com/a", context.DeadlineExceeded),
		want: "the fetch ran out of time",
	}, {
		name: "a bare deadline reads as words",
		err:  context.DeadlineExceeded,
		want: "the fetch ran out of time",
	}, {
		// Everything else is already written for a reader — "HTTP 403", a DNS
		// failure — and paraphrasing it would lose the only detail that says
		// which host to go and look at.
		name: "any other failure keeps its own words",
		err:  errors.New("dial tcp: no such host"),
		want: "dial tcp: no such host",
	}, {
		// Cancellation is not a deadline. It reaches the attention queue only if
		// something upstream decided to record it anyway, and it must not then
		// claim the page was slow.
		name: "cancellation is not running out of time",
		err:  context.Canceled,
		want: "context canceled",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := describe(c.err, "the fetch ran out of time"); got != c.want {
				t.Errorf("describe(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// interrupted decides whether an outcome may be recorded at all, so it is worth
// proving it separates the two ways a job's context can be finished.
func TestInterruptedTellsAShutdownFromRunningOutOfTime(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"a canceled context is a shutdown", canceled, true},
		{"an expired deadline is not", expired, false},
		{"a live context is neither", context.Background(), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := interrupted(c.ctx); got != c.want {
				t.Errorf("interrupted() = %v, want %v (ctx.Err() = %v)", got, c.want, c.ctx.Err())
			}
		})
	}
}

// The point of recording() is that it works when its parent does not.
func TestRecordingOutlivesTheJobsOwnDeadline(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if expired.Err() == nil {
		t.Fatal("the parent context should already be done; the test proves nothing otherwise")
	}

	recCtx, cancelRec := recording(expired)
	defer cancelRec()

	if err := recCtx.Err(); err != nil {
		t.Fatalf("recording(expired).Err() = %v, want nil — a write that cannot run records nothing", err)
	}

	// Bounded, not unbounded: this runs on the pool the rest of the worker shares.
	deadline, ok := recCtx.Deadline()
	if !ok {
		t.Fatal("recording() returned a context with no deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > recordTimeout {
		t.Errorf("recording() has %v left, want a positive value no greater than %v", remaining, recordTimeout)
	}

	// Cancellation still propagates from the returned cancel func, so a caller
	// that returns cannot leak the context it made.
	cancelRec()
	if !errors.Is(recCtx.Err(), context.Canceled) {
		t.Errorf("after cancel, Err() = %v, want context.Canceled", recCtx.Err())
	}
}

// A fetch that uses up the job's whole time budget still has to say so.
//
// This is the bug the package comment describes, reproduced at the level it
// actually bit: the failure was recorded through the job's own context, so the
// one outcome that could not be written down was the one where the time ran out.
// The article then sat 'pending' with no fetch_error — a state the attention
// queue does not select and the fetch scheduler re-enqueues forever.
//
// Work is called directly rather than through the queue because what is being
// proved is an effect on one row, and River's retry would otherwise decide how
// many times it happened.
func TestAFetchThatRunsOutOfTimeRecordsTheFailure(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	// robots.txt answers immediately so that the deadline is spent on the page.
	// A hang here would prove the same thing by a different route and make the
	// test's subject ambiguous.
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		<-blocked
	}))
	defer srv.Close()
	defer close(blocked)

	worker := &FetchArticleWorker{
		store:  s,
		client: httpclient.New(httpclient.Options{UserAgent: "tomekeeper/test", MaxAttempts: 1, DefaultRPS: 100}),
		blobs:  blobs,
		log:    slog.New(slog.DiscardHandler),
	}

	id, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: srv.URL + "/slow",
		URLOriginal:  srv.URL + "/slow",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	// Standing in for River's JobTimeout, which is one minute by default and is
	// not configured here. The duration only has to be shorter than the page.
	jobCtx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	err = worker.Work(jobCtx, &river.Job[FetchArticleArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   FetchArticleArgs{ArticleID: int64(id)},
	})
	if err != nil {
		t.Errorf("Work() = %v — the failure was recorded, so there is nothing left to retry", err)
	}

	// Read on a fresh context: the job's is expired, which is the whole point.
	a, err := s.GetArticle(t.Context(), id)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if a.FetchStatus != store.FetchFailed {
		t.Errorf("fetch_status = %q, want %q — 'pending' is invisible to the attention queue",
			a.FetchStatus, store.FetchFailed)
	}
	if a.FetchError == "" {
		t.Error("fetch_error is empty; an article with no reason recorded is one nobody can act on")
	}
	if a.FetchError != "the fetch ran out of time" {
		t.Errorf("fetch_error = %q, want the readable deadline text", a.FetchError)
	}
}

// A worker shutting down mid-fetch must not blame the page for it.
//
// The counterweight to the test above, and the reason interrupted() exists:
// River hands an interrupted job to the next worker that starts, so recording a
// failure here would permanently fail whatever was in flight during every
// rolling restart — permanently, because a recorded failure is never retried.
func TestAFetchInterruptedByAShutdownIsLeftAlone(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	// The page hangs until the fetch is canceled, so cancellation is what ends
	// the request rather than a race between two timers.
	arrived := make(chan struct{})
	blocked := make(chan struct{})
	// Once, because the handler runs on the server's goroutines and a retry would
	// otherwise close a closed channel.
	var announce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		announce.Do(func() { close(arrived) })
		<-blocked
	}))
	defer srv.Close()
	defer close(blocked)

	worker := &FetchArticleWorker{
		store:  s,
		client: httpclient.New(httpclient.Options{UserAgent: "tomekeeper/test", MaxAttempts: 1, DefaultRPS: 100}),
		blobs:  blobs,
		log:    slog.New(slog.DiscardHandler),
	}

	id, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: srv.URL + "/interrupted",
		URLOriginal:  srv.URL + "/interrupted",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	jobCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		// Cancel only once the request is in flight. Canceling earlier would be
		// caught by the article lookup at the top of Work, which is a different
		// path and would make this pass for the wrong reason.
		<-arrived
		cancel()
	}()

	err = worker.Work(jobCtx, &river.Job[FetchArticleArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   FetchArticleArgs{ArticleID: int64(id)},
	})
	if err == nil {
		t.Error("Work() = nil; an interrupted job must be returned for retry, not treated as finished")
	}

	a, err := s.GetArticle(t.Context(), id)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if a.FetchStatus != store.FetchPending {
		t.Errorf("fetch_status = %q, want %q — our shutdown is not the page's failure",
			a.FetchStatus, store.FetchPending)
	}
	if a.FetchError != "" {
		t.Errorf("fetch_error = %q, want empty — nothing about this article has been decided", a.FetchError)
	}
}
