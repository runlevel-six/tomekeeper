package feed_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/feed"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The whole M1 pipeline against a live database: subscribe, poll, and confirm
// that a second poll of an unchanged feed transfers nothing and adds nothing.
//
// Skips without TOME_TEST_DATABASE_URL. See internal/dbtest.
func TestPollPipelineAgainstDatabase(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	var requests, conditionalRequests int
	const etag = `"v1"`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++

		if r.Header.Get("If-None-Match") == etag {
			conditionalRequests++
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, rssTwoItems)
	}))
	defer srv.Close()

	feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: srv.URL, Title: "Example Blog", Category: "Tech",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	poller := feed.NewPoller(s, httpclient.New(httpclient.Options{UserAgent: "tomekeeper/test", MaxAttempts: 1}),
		feed.DefaultIntervalPolicy(), 20, discardLogger())

	first, err := poller.Poll(ctx, userID, feedID)
	if err != nil {
		t.Fatalf("first Poll() = %v", err)
	}
	if first.NewArticles != 2 || first.NewItems != 2 {
		t.Fatalf("first poll: %d new articles, %d new items; want 2 and 2",
			first.NewArticles, first.NewItems)
	}

	// The validator must have been persisted, or the second poll cannot be a
	// conditional request.
	f, err := s.GetFeed(ctx, userID, feedID)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if f.ETag != etag {
		t.Fatalf("stored ETag = %q, want %q", f.ETag, etag)
	}

	// Captured because the first poll found items, and OnNewItems halves the
	// interval rather than resetting it to the floor. Growth from the 304 has to
	// be measured against this, not against the one-hour default the feed was
	// created with — the interval is legitimately below that by now.
	afterFirstPoll := f.PollInterval

	// A due time in the future is exactly right in production, but this test
	// polls again immediately.
	second, err := poller.Poll(ctx, userID, feedID)
	if err != nil {
		t.Fatalf("second Poll() = %v", err)
	}

	if !second.NotModified {
		t.Error("the second poll was not a 304; the conditional request did not work")
	}
	if conditionalRequests != 1 {
		t.Errorf("the server saw %d conditional requests, want 1", conditionalRequests)
	}
	if second.NewArticles != 0 || second.NewItems != 0 {
		t.Errorf("second poll added %d articles and %d items, want 0 and 0",
			second.NewArticles, second.NewItems)
	}

	count, err := s.CountArticles(ctx)
	if err != nil {
		t.Fatalf("CountArticles() = %v", err)
	}
	if count != 2 {
		t.Errorf("the archive holds %d articles after two polls, want 2", count)
	}

	// M1 stops here: articles are left pending for M2's fetcher.
	article, err := s.GetArticleByURL(ctx, "https://example.com/first")
	if err != nil {
		t.Fatalf("GetArticleByURL() = %v", err)
	}
	if article.FetchStatus != "pending" {
		t.Errorf("FetchStatus = %q, want %q", article.FetchStatus, "pending")
	}
	if article.Title != "First Post" {
		t.Errorf("Title = %q, want %q", article.Title, "First Post")
	}
	if article.PublishedAt == nil {
		t.Error("PublishedAt is nil; the feed supplied a pubDate")
	}

	// A 304 is a success, so the interval grows and the failure count stays at
	// zero — a quiet feed is not a broken one.
	f, err = s.GetFeed(ctx, userID, feedID)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if f.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d after two successful polls, want 0", f.ConsecutiveFailures)
	}
	if f.PollInterval <= afterFirstPoll {
		t.Errorf("PollInterval = %v after a 304, want it to have grown past the %v the first poll left",
			f.PollInterval, afterFirstPoll)
	}
}

// The same story carried by two different feeds must produce one article and
// two references, against the real unique constraints rather than a double.
func TestSyndicatedStoryCollapsesInDatabase(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	const shared = "https://example.com/the-same-story"

	body := func(guid, link string) string {
		return `<?xml version="1.0"?><rss version="2.0"><channel><title>F</title>` +
			`<item><title>Shared</title><link>` + link + `</link><guid>` + guid + `</guid></item>` +
			`</channel></rss>`
	}

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body("a-1", shared))
	}))
	defer srvA.Close()

	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The same article, decorated differently by the syndicating feed.
		_, _ = io.WriteString(w, body("b-1", shared+"/?utm_source=b&fbclid=xyz"))
	}))
	defer srvB.Close()

	poller := feed.NewPoller(s, httpclient.New(httpclient.Options{UserAgent: "tomekeeper/test", MaxAttempts: 1}),
		feed.DefaultIntervalPolicy(), 20, discardLogger())

	for _, url := range []string{srvA.URL, srvB.URL} {
		feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{FeedURL: url, Title: url})
		if err != nil {
			t.Fatalf("UpsertFeed() = %v", err)
		}
		if _, err := poller.Poll(ctx, userID, feedID); err != nil {
			t.Fatalf("Poll() = %v", err)
		}
	}

	count, err := s.CountArticles(ctx)
	if err != nil {
		t.Fatalf("CountArticles() = %v", err)
	}
	if count != 1 {
		t.Errorf("the archive holds %d articles, want 1 — the duplicate did not collapse", count)
	}

	// One article, reachable through both subscriptions.
	reachable, err := s.CountUserArticles(ctx, userID)
	if err != nil {
		t.Fatalf("CountUserArticles() = %v", err)
	}
	if reachable != 1 {
		t.Errorf("the user can reach %d distinct articles, want 1", reachable)
	}
}
