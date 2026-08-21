package jobs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/jobs"
	"github.com/runlevel-six/tomekeeper/internal/render"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The handoff from fetching to rendering.
//
// No browser is involved in any of this, deliberately. What is being tested is the
// *decision* — whether a domain flagged as needing JavaScript is routed to the render
// queue instead of being fetched plainly — and that decision is worth testing on its
// own, because `requires_js` was a column an operator could set, a form could save and
// a CLI could print, and which nothing read at all for three milestones. The browser's
// own behavior is tested in internal/render against a real Chrome.

// countJobs returns how many jobs of a kind are in the queue, in any state.
func countJobs(ctx context.Context, t *testing.T, s *store.Store, kind string) int {
	t.Helper()

	var n int
	err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = $1`, kind).Scan(&n)
	if err != nil {
		t.Fatalf("counting %s jobs: %v", kind, err)
	}
	return n
}

// queueOf returns the queue a kind's jobs were inserted onto.
func queueOf(t *testing.T, s *store.Store, kind string) string {
	t.Helper()

	var queue string
	err := s.Pool().QueryRow(t.Context(),
		`SELECT queue FROM river_job WHERE kind = $1 LIMIT 1`, kind).Scan(&queue)
	if err != nil {
		t.Fatalf("reading the queue for %s: %v", kind, err)
	}
	return queue
}

// A flagged domain is handed to the render queue, and the page is not fetched.
//
// The server would answer — it is a live httptest server — so the assertion that it
// was never asked is what proves the handoff happened rather than the fetch merely
// having succeeded first.
func TestAFlaggedDomainIsHandedToTheRenderQueue(t *testing.T) {
	pool, s, _ := dbtest.SetupWithUser(t)
	_ = pool

	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		asked++
		_, _ = w.Write([]byte("<html><body><div id=app></div></body></html>"))
	}))
	defer srv.Close()

	ctx := t.Context()
	id := newFetchableArticle(t, s, srv.URL+"/needs-js")

	// The flag, set the way an operator sets it.
	if err := s.System().UpsertDomainRule(ctx, store.DomainRule{
		Domain: hostOfURL(t, srv.URL), RequiresJS: true,
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	// A renderer is configured, so the handoff is possible. It points nowhere: this
	// test never lets a render run, it only checks that one was queued.
	runPipelineWithRenderer(t, s, "ws://127.0.0.1:1", func(ctx context.Context, rc *river.Client[pgx.Tx]) {
		if _, err := rc.Insert(ctx, jobs.FetchArticleArgs{ArticleID: int64(id)}, nil); err != nil {
			t.Fatalf("inserting the fetch: %v", err)
		}

		waitFor(t, "a render to be queued for a domain flagged as needing JavaScript",
			func() bool { return countJobs(ctx, t, s, "render_article") > 0 })
	})

	if asked != 0 {
		t.Errorf("the page was fetched %d time(s) despite the domain being flagged; the "+
			"handoff did not happen", asked)
	}

	// On its own queue, which is what keeps a hung browser from consuming the pool that
	// polls feeds.
	if got := queueOf(t, s, "render_article"); got != jobs.RenderQueue {
		t.Errorf("the render was queued on %q, want %q", got, jobs.RenderQueue)
	}
}

// An unflagged domain is fetched, and nothing is queued for a browser.
//
// The counterweight: making the flag work must not route everything through a
// browser, and a test that only asserted the positive case would pass if the
// condition were inverted or ignored.
func TestAnUnflaggedDomainIsFetchedNormally(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("<html><body><article><p>" +
			"A plain page with enough prose in it to clear the extractor's floor, which " +
			"needs a couple of hundred characters before it will call something an " +
			"article rather than a fragment of navigation furniture.</p></article></body></html>"))
	}))
	defer srv.Close()

	id := newFetchableArticle(t, s, srv.URL+"/plain")

	// A rule that *exists* and is simply not flagged, which is the whole design of this
	// test. Without one, the lookup returns not-found and the handoff exits before it
	// ever reads requires_js — so the assertion passed with the flag check deleted, and
	// tested nothing. A host with a selector and no JS flag is the case that reaches it.
	if err := s.System().UpsertDomainRule(t.Context(), store.DomainRule{
		Domain: hostOfURL(t, srv.URL), ContentSelector: "article", RequiresJS: false,
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	runPipelineWithRenderer(t, s, "ws://127.0.0.1:1", func(ctx context.Context, rc *river.Client[pgx.Tx]) {
		if _, err := rc.Insert(ctx, jobs.FetchArticleArgs{ArticleID: int64(id)}, nil); err != nil {
			t.Fatalf("inserting the fetch: %v", err)
		}

		waitFor(t, "an unflagged article to be fetched", func() bool {
			a, err := s.GetArticle(ctx, id)
			return err == nil && a.FetchStatus == store.FetchOK
		})
	})

	if n := countJobs(t.Context(), t, s, "render_article"); n != 0 {
		t.Errorf("%d render(s) were queued for a domain nobody flagged", n)
	}

	a, err := s.GetArticle(t.Context(), id)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	// The column records how the page was obtained, and a plain fetch has to say so —
	// otherwise it is another column nothing ever writes correctly.
	if a.BrowserRendered {
		t.Error("a plainly fetched article claims it was rendered by a browser")
	}
}

// With no browser configured, a flagged domain is fetched plainly rather than stalling.
//
// This is the configuration mistake case: somebody flagged a domain and never deployed
// a browser. Storing the shell is a poor archive copy, but it is better than an article
// that never arrives, and it lands in the attention queue where the mistake is visible.
func TestAFlaggedDomainWithNoBrowserIsStillFetched(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("<html><body><div id=app></div></body></html>"))
	}))
	defer srv.Close()

	ctx := t.Context()
	id := newFetchableArticle(t, s, srv.URL+"/needs-js-no-browser")
	if err := s.System().UpsertDomainRule(ctx, store.DomainRule{
		Domain: hostOfURL(t, srv.URL), RequiresJS: true,
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	// No renderer at all, which is the default deployment.
	runPipelineWithRenderer(t, s, "", func(ctx context.Context, rc *river.Client[pgx.Tx]) {
		if _, err := rc.Insert(ctx, jobs.FetchArticleArgs{ArticleID: int64(id)}, nil); err != nil {
			t.Fatalf("inserting the fetch: %v", err)
		}

		waitFor(t, "a flagged article with no browser to be resolved either way", func() bool {
			a, err := s.GetArticle(ctx, id)
			return err == nil && a.FetchStatus != store.FetchPending
		})
	})

	if n := countJobs(t.Context(), t, s, "render_article"); n != 0 {
		t.Errorf("%d render(s) were queued with no browser configured", n)
	}
}

// newFetchableArticle inserts an article for the pipeline to work on.
func newFetchableArticle(t *testing.T, s *store.Store, canonical string) store.ArticleID {
	t.Helper()

	id, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: canonical, Title: "A Page That Needs Deciding About",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	return id
}

// hostOfURL is the host a test server is listening on, which is what a domain rule has
// to name.
func hostOfURL(t *testing.T, rawURL string) string {
	t.Helper()

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", rawURL, err)
	}
	return u.Hostname()
}

// runPipelineWithRenderer starts a worker whose renderer points at browserURL.
//
// Separate from runPipeline rather than a parameter on it, so that every existing test
// keeps starting a worker with no browser — which is the deployment nearly everybody
// has, and the configuration those tests were written against.
func runPipelineWithRenderer(t *testing.T, s *store.Store, browserURL string,
	fn func(context.Context, *river.Client[pgx.Tx]),
) {
	t.Helper()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}
	client := httpclient.New(httpclient.Options{
		UserAgent: "tomekeeper/test", MaxAttempts: 1, DefaultRPS: 100,
	})

	renderer, err := render.New(render.Options{
		WebSocketURL: browserURL, UserAgent: "tomekeeper/test", Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("render.New() = %v", err)
	}

	runPipelineWith(t, s, blobs, client, renderer, fn)
}

// A flagged domain with a browser configured but unreachable *says* it is waiting.
//
// The state this asserts did not exist until 2026-08-21, and its absence was the whole
// problem: the article sat pending forever, retried every minute, invisible to the
// failed-fetch queue and badged "queued" in the reading list with a tooltip claiming the
// worker had not reached it. Three things have to hold together — the status stays
// pending so the scheduler keeps trying, the reason is recorded so somebody can see why,
// and the queue that exists for things needing attention actually lists it.
func TestAFlaggedArticleWithNoBrowserSaysItIsWaiting(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("<html><body><div id=app></div></body></html>"))
	}))
	defer srv.Close()

	feedID, _, err := s.UpsertFeed(ctx, alice, store.FeedParams{
		FeedURL: srv.URL + "/feed.xml", Title: "Needs JS",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	id := newFetchableArticle(t, s, srv.URL+"/waiting")
	if _, err := s.InsertFeedItem(ctx, alice, store.FeedItemParams{
		FeedID: feedID, ArticleID: id, GUID: "guid-waiting",
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
	if err := s.System().UpsertDomainRule(ctx, store.DomainRule{
		Domain: hostOfURL(t, srv.URL), RequiresJS: true,
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	// A browser is configured and answers nothing, which is what a Deployment scaled to
	// zero looks like from here.
	runPipelineWithRenderer(t, s, "ws://127.0.0.1:1", func(ctx context.Context, rc *river.Client[pgx.Tx]) {
		if _, err := rc.Insert(ctx, jobs.FetchArticleArgs{ArticleID: int64(id)}, nil); err != nil {
			t.Fatalf("inserting the fetch: %v", err)
		}

		waitFor(t, "the article to be marked as waiting", func() bool {
			a, err := s.GetArticle(ctx, id)
			return err == nil && a.FetchError != ""
		})
	})

	a, err := s.GetArticle(ctx, id)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	// Pending, not failed: an operator's deployment is not the site's fault, and a
	// recorded failure would never be retried.
	if a.FetchStatus != store.FetchPending {
		t.Errorf("fetch_status = %q, want %q — a missing browser must stay retryable",
			a.FetchStatus, store.FetchPending)
	}
	if !strings.Contains(a.FetchError, "browser") {
		t.Errorf("fetch_error = %q, want it to name the browser", a.FetchError)
	}

	// And it is visible where somebody looks for things that need attention.
	rows, err := s.NeedsAttentionFor(ctx, alice, 50)
	if err != nil {
		t.Fatalf("NeedsAttentionFor() = %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.ArticleID == id {
			found = true
			if r.FetchStatus != store.FetchPending {
				t.Errorf("the attention row says %q", r.FetchStatus)
			}
		}
	}
	if !found {
		t.Errorf("an article waiting for a browser is missing from the attention queue (%d rows)",
			len(rows))
	}
}

// An article nobody has reached yet stays out of the attention queue.
//
// The counterweight, and the reason the predicate is "pending *with a reason*" rather
// than "pending": every article is pending for a moment after it is created, and a queue
// that listed all of them would list the whole archive on a first import.
func TestAnUnreachedArticleIsNotInTheAttentionQueue(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	feedID, _, err := s.UpsertFeed(ctx, alice, store.FeedParams{
		FeedURL: "https://example.com/feed.xml", Title: "Ordinary",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	id := newFetchableArticle(t, s, "https://example.com/not-yet-fetched")
	if _, err := s.InsertFeedItem(ctx, alice, store.FeedItemParams{
		FeedID: feedID, ArticleID: id, GUID: "guid-not-yet",
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}

	rows, err := s.NeedsAttentionFor(ctx, alice, 50)
	if err != nil {
		t.Fatalf("NeedsAttentionFor() = %v", err)
	}
	for _, r := range rows {
		if r.ArticleID == id {
			t.Error("a freshly created article appears in the attention queue; on a first " +
				"import that would list the entire archive")
		}
	}
}
