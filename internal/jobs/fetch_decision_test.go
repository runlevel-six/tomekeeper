package jobs

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// A page this archive already has is not fetched again unless somebody asked, and
// this calls Work directly to prove it.
//
// The guard matters because the pipeline enqueues fetches freely — three feeds
// carrying one story, a retry, a scheduler sweep that runs on every worker start —
// and any of those re-fetching would be this archive spending somebody else's
// bandwidth on a page it already had.
//
// Synchronous on purpose, and the previous attempt is the argument for it. This ran
// through the job queue: enqueue an ordinary fetch for an article that has one, wait,
// assert the request count did not move. But fetches are unique per article across
// every non-terminal state, so the insert is refused outright while another is
// outstanding — and then the assertion passes because *River* dropped the job, not
// because the worker declined. The test detected that case and failed rather than
// claiming a pass, which is why CI failed about half the time and every re-run went
// green.
//
// Waiting harder would not have fixed it. The precondition — nothing outstanding for
// this article — is one the test can observe but cannot create, because the worker's
// own periodic sweep enqueues fetches for every article awaiting one. A refusal is the
// absence of an effect, and a queue is the wrong place to look for one.
func TestAnAlreadyFetchedPageIsLeftAlone(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	// Counted with atomics because the handler runs on the server's goroutine, and a
	// test whose instrument races is not evidence of anything.
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// robots.txt is excluded because the client fetches it before the page, and
		// counting it would make a declined fetch look like one request rather than
		// none. Nothing here should reach the server at all.
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><p>Served.</p></body></html>`))
	}))
	defer srv.Close()

	worker := &FetchArticleWorker{
		store:  s,
		client: httpclient.New(httpclient.Options{UserAgent: "tomekeeper/test", MaxAttempts: 1, DefaultRPS: 100}),
		blobs:  blobs,
		log:    slog.New(slog.DiscardHandler),
	}

	id, _, err := s.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: srv.URL + "/notes",
		URLOriginal:  srv.URL + "/notes",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	// A stored page, which is the whole precondition: the refusal is about an article
	// that already has bytes, and RecordFetchSuccess is what puts it in that state.
	const stored = "articles/test/notes/raw.html.gz"
	if err := s.RecordFetchSuccess(ctx, id, store.FetchedPage{SHA: "the-stored-page", Path: stored}); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}

	// Again is false, which is what the poller, the scheduler and a retry all send.
	// There is no helper that sets it — EnqueueRefetch is the only caller that does,
	// and that is the point.
	err = worker.Work(ctx, &river.Job[FetchArticleArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   FetchArticleArgs{ArticleID: int64(id)},
	})
	if err != nil {
		t.Fatalf("Work() = %v — declining is not an error, it is the ordinary case", err)
	}

	if n := requests.Load(); n != 0 {
		t.Errorf("an ordinary fetch of an already-fetched page made %d request(s) to the origin", n)
	}

	// The stored page is also untouched, which is the half a request count cannot see:
	// a fetch that happened and then wrote the same path would still be a request the
	// origin did not need to serve, and a fetch that rewrote the article's pointer
	// would strand the bytes it already had.
	a, err := s.GetArticle(ctx, id)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if a.RawBlobSHA != "the-stored-page" {
		t.Errorf("the stored page changed: raw_blob_sha is %q, want the original", a.RawBlobSHA)
	}
	if a.RawBlobPath != stored {
		t.Errorf("the article points at %q, want the page it already had at %q", a.RawBlobPath, stored)
	}

	// No extraction was enqueued either — which this proves by omission rather than by
	// assertion, and the omission is load-bearing: there is no river client in this
	// context, so the tail of Work that enqueues extraction would have returned "no
	// river client in context" if it had been reached. The nil error above is that
	// check.
}
