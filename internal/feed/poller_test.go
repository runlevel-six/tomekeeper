package feed_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/feed"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

const (
	testUserID = store.UserID(1)
	testFeedID = store.FeedID(42)
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newPoller wires a Poller against a test server and an in-memory store.
func newPoller(t *testing.T, s *fakeStore) *feed.Poller {
	t.Helper()
	return feed.NewPoller(s, httpclient.New(httpclient.Options{UserAgent: "tomekeeper/test", MaxAttempts: 1}),
		feed.DefaultIntervalPolicy(), 20, discardLogger())
}

func testFeed(feedURL string) store.Feed {
	return store.Feed{
		ID:           testFeedID,
		UserID:       testUserID,
		FeedURL:      feedURL,
		Title:        "Test Feed",
		PollInterval: time.Hour,
	}
}

const rssTwoItems = `<?xml version="1.0"?>
<rss version="2.0">
  <channel>
    <title>Example Blog</title>
    <link>https://example.com/</link>
    <item>
      <title>First Post</title>
      <link>https://example.com/first</link>
      <guid>https://example.com/first</guid>
      <pubDate>Mon, 03 Aug 2026 10:00:00 GMT</pubDate>
      <description>A summary.</description>
    </item>
    <item>
      <title>Second Post</title>
      <link>https://example.com/second</link>
      <guid>tag:example.com,2026:second</guid>
      <pubDate>Tue, 04 Aug 2026 10:00:00 GMT</pubDate>
    </item>
  </channel>
</rss>`

func TestPollIngestsItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("Last-Modified", "Mon, 03 Aug 2026 10:00:00 GMT")
		_, _ = io.WriteString(w, rssTwoItems)
	}))
	defer srv.Close()

	fake := newFakeStore(testFeed(srv.URL))
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}

	if got, want := res.TotalItems, 2; got != want {
		t.Errorf("TotalItems = %d, want %d", got, want)
	}
	if got, want := res.NewItems, 2; got != want {
		t.Errorf("NewItems = %d, want %d", got, want)
	}
	if got, want := res.NewArticles, 2; got != want {
		t.Errorf("NewArticles = %d, want %d", got, want)
	}

	// The conditional-GET validators must be stored, or the next poll cannot
	// produce a 304 and the whole politeness story collapses.
	if len(fake.successes) != 1 {
		t.Fatalf("recorded %d successes, want 1", len(fake.successes))
	}
	if got, want := fake.successes[0].ETag, `"abc123"`; got != want {
		t.Errorf("stored ETag = %q, want %q", got, want)
	}
	if got, want := fake.successes[0].LastModified, "Mon, 03 Aug 2026 10:00:00 GMT"; got != want {
		t.Errorf("stored Last-Modified = %q, want %q", got, want)
	}
}

// The validators must go back out on the next request.
func TestPollSendsConditionalHeaders(t *testing.T) {
	var gotINM, gotIMS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotINM = r.Header.Get("If-None-Match")
		gotIMS = r.Header.Get("If-Modified-Since")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	f := testFeed(srv.URL)
	f.ETag = `"abc123"`
	f.LastModified = "Mon, 03 Aug 2026 10:00:00 GMT"

	fake := newFakeStore(f)
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}

	if !res.NotModified {
		t.Error("NotModified = false, want true for a 304 response")
	}
	if gotINM != `"abc123"` {
		t.Errorf("If-None-Match = %q, want %q", gotINM, `"abc123"`)
	}
	if gotIMS != "Mon, 03 Aug 2026 10:00:00 GMT" {
		t.Errorf("If-Modified-Since = %q, want the stored value", gotIMS)
	}

	// A 304 is a success, and it should lengthen the interval.
	if len(fake.notModified) != 1 {
		t.Fatalf("recorded %d 304s, want 1", len(fake.notModified))
	}
	if got := fake.notModified[0]; got <= time.Hour {
		t.Errorf("interval after 304 = %v, want longer than the previous hour", got)
	}
	if len(fake.failures) != 0 {
		t.Errorf("a 304 recorded %d failures, want 0", len(fake.failures))
	}
}

// A feed with no stored validators must not send empty ones — some origins
// treat If-None-Match: "" as a match and answer 304 forever, which would make
// the feed permanently invisible.
func TestPollOmitsEmptyConditionalHeaders(t *testing.T) {
	var hasINM, hasIMS bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasINM = r.Header["If-None-Match"]
		_, hasIMS = r.Header["If-Modified-Since"]
		_, _ = io.WriteString(w, rssTwoItems)
	}))
	defer srv.Close()

	if _, err := newPoller(t, newFakeStore(testFeed(srv.URL))).Poll(t.Context(), testUserID, testFeedID); err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	if hasINM {
		t.Error("If-None-Match was sent with no stored ETag")
	}
	if hasIMS {
		t.Error("If-Modified-Since was sent with no stored Last-Modified")
	}
}

// The M1 acceptance criterion: a second poll of an unchanged feed produces no
// new articles.
func TestSecondPollOfUnchangedFeedAddsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, rssTwoItems)
	}))
	defer srv.Close()

	fake := newFakeStore(testFeed(srv.URL))
	poller := newPoller(t, fake)

	first, err := poller.Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("first Poll() = %v", err)
	}
	second, err := poller.Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("second Poll() = %v", err)
	}

	if first.NewArticles != 2 {
		t.Fatalf("first poll found %d new articles, want 2", first.NewArticles)
	}
	if second.NewArticles != 0 {
		t.Errorf("second poll of an unchanged feed found %d new articles, want 0", second.NewArticles)
	}
	if second.NewItems != 0 {
		t.Errorf("second poll found %d new references, want 0", second.NewItems)
	}
	if got, want := fake.articleCount(), 2; got != want {
		t.Errorf("store holds %d articles, want %d", got, want)
	}
}

// The property that makes an article the root entity: the same story listed by
// two feeds with different tracking decoration is one article.
func TestDuplicateArticleAcrossFeedsCollapses(t *testing.T) {
	const feedA = `<?xml version="1.0"?><rss version="2.0"><channel><title>A</title>
	  <item><title>Shared</title><link>https://example.com/shared</link><guid>a-1</guid></item>
	</channel></rss>`
	const feedB = `<?xml version="1.0"?><rss version="2.0"><channel><title>B</title>
	  <item><title>Shared</title><link>https://example.com/shared/?utm_source=b</link><guid>b-1</guid></item>
	</channel></rss>`

	bodies := []string{feedA, feedB}
	var index int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, bodies[index])
	}))
	defer srv.Close()

	fake := newFakeStore(testFeed(srv.URL))
	poller := newPoller(t, fake)

	if _, err := poller.Poll(t.Context(), testUserID, testFeedID); err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	index = 1
	second, err := poller.Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}

	if second.NewArticles != 0 {
		t.Errorf("the same story via a second feed created %d articles, want 0", second.NewArticles)
	}
	if second.NewItems != 1 {
		t.Errorf("the second feed created %d references, want 1", second.NewItems)
	}
	if got, want := fake.articleCount(), 1; got != want {
		t.Errorf("store holds %d articles, want %d — the duplicate did not collapse", got, want)
	}
}

// A feed listing the same entry twice in one document must not produce two
// references.
func TestDuplicateGUIDWithinOnePollIsIgnored(t *testing.T) {
	const body = `<?xml version="1.0"?><rss version="2.0"><channel><title>Dup</title>
	  <item><title>One</title><link>https://example.com/one</link><guid>same</guid></item>
	  <item><title>One again</title><link>https://example.com/one</link><guid>same</guid></item>
	</channel></rss>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	fake := newFakeStore(testFeed(srv.URL))
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}

	if got, want := res.TotalItems, 2; got != want {
		t.Errorf("TotalItems = %d, want %d", got, want)
	}
	if got, want := res.NewItems, 1; got != want {
		t.Errorf("NewItems = %d, want %d — the repeated GUID was not deduplicated", got, want)
	}
}

// An entry with no GUID falls back to its canonical URL as the key.
func TestMissingGUIDFallsBackToCanonicalURL(t *testing.T) {
	const body = `<?xml version="1.0"?><rss version="2.0"><channel><title>NoGUID</title>
	  <item><title>One</title><link>https://example.com/one</link></item>
	  <item><title>One with tracking</title><link>https://example.com/one?utm_source=x</link></item>
	</channel></rss>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	fake := newFakeStore(testFeed(srv.URL))
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}

	if got, want := res.NewItems, 1; got != want {
		t.Errorf("NewItems = %d, want %d — the two links are the same article", got, want)
	}
}

// Relative entry links are resolved against the feed's site URL. Feeds carry
// these more often than the specifications suggest, and an unresolved relative
// link cannot be canonicalized, so these entries would otherwise vanish.
func TestRelativeLinksAreResolved(t *testing.T) {
	const body = `<?xml version="1.0"?><rss version="2.0"><channel>
	  <title>Relative</title><link>https://blog.example.com/</link>
	  <item><title>Post</title><link>/posts/hello</link><guid>rel-1</guid></item>
	</channel></rss>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	f := testFeed(srv.URL)
	f.SiteURL = "https://blog.example.com/"

	fake := newFakeStore(f)
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}

	if res.NewArticles != 1 {
		t.Fatalf("NewArticles = %d, want 1", res.NewArticles)
	}
	if !fake.hasArticle("https://blog.example.com/posts/hello") {
		t.Error("the relative link was not resolved against the site URL")
	}
}

// One unusable entry must not abort a poll that also contains good ones.
func TestUnusableLinkSkipsOnlyThatEntry(t *testing.T) {
	const body = `<?xml version="1.0"?><rss version="2.0"><channel><title>Mixed</title>
	  <item><title>Good</title><link>https://example.com/good</link><guid>g</guid></item>
	  <item><title>No link</title><guid>n</guid></item>
	  <item><title>Bad scheme</title><link>javascript:alert(1)</link><guid>b</guid></item>
	  <item><title>Also good</title><link>https://example.com/also</link><guid>a</guid></item>
	</channel></rss>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	fake := newFakeStore(testFeed(srv.URL))
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v, want the poll to survive unusable entries", err)
	}

	if got, want := res.NewArticles, 2; got != want {
		t.Errorf("NewArticles = %d, want %d", got, want)
	}
}

func TestPollRecordsHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	fake := newFakeStore(testFeed(srv.URL))

	// A site being unavailable is operational reality, not a job error: the
	// failure is recorded on the feed and the poll returns nil.
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v, want the failure recorded rather than returned", err)
	}
	if res.Disabled {
		t.Error("a single failure disabled the feed")
	}

	if len(fake.failures) != 1 {
		t.Fatalf("recorded %d failures, want 1", len(fake.failures))
	}
	if got := fake.failures[0].Cause; !strings.Contains(got, "429") {
		t.Errorf("failure cause = %q, want it to name the status code", got)
	}
	if got := fake.failures[0].Cause; !strings.Contains(got, "Retry-After") {
		t.Errorf("failure cause = %q, want it to preserve Retry-After", got)
	}
}

func TestPollRecordsMalformedFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<rss><channel><title>Truncated")
	}))
	defer srv.Close()

	fake := newFakeStore(testFeed(srv.URL))
	if _, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID); err != nil {
		t.Fatalf("Poll() = %v, want the parse failure recorded rather than returned", err)
	}

	if len(fake.failures) != 1 {
		t.Fatalf("recorded %d failures, want 1", len(fake.failures))
	}
	if got := fake.failures[0].Cause; !strings.Contains(got, "parsing feed") {
		t.Errorf("failure cause = %q, want it to say parsing failed", got)
	}
}

// The failure that disables a feed must say so, so the UI can surface it.
func TestFeedIsDisabledAtThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := testFeed(srv.URL)
	f.ConsecutiveFailures = 19 // the next failure is the twentieth

	fake := newFakeStore(f)
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}

	if !res.Disabled {
		t.Error("Disabled = false on the twentieth consecutive failure, want true")
	}
	if got, want := fake.failures[0].DisableAfter, 20; got != want {
		t.Errorf("DisableAfter = %d, want %d", got, want)
	}
}

func TestDisabledFeedIsNotPolled(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = io.WriteString(w, rssTwoItems)
	}))
	defer srv.Close()

	f := testFeed(srv.URL)
	f.Disabled = true

	res, err := newPoller(t, newFakeStore(f)).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	if !res.Skipped {
		t.Error("Skipped = false for a disabled feed, want true")
	}
	if requests != 0 {
		t.Errorf("a disabled feed was fetched %d times, want 0", requests)
	}
}

// A feed that declares its own cadence is believed over the inferred one.
func TestSyUpdatePeriodOverridesAdaptiveInterval(t *testing.T) {
	const body = `<?xml version="1.0"?>
	<rss version="2.0" xmlns:sy="http://purl.org/rss/1.0/modules/syndication/">
	  <channel>
	    <title>Declared</title>
	    <sy:updatePeriod>hourly</sy:updatePeriod>
	    <sy:updateFrequency>2</sy:updateFrequency>
	    <item><title>Post</title><link>https://example.com/p</link><guid>p</guid></item>
	  </channel>
	</rss>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	fake := newFakeStore(testFeed(srv.URL))
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}

	if got, want := res.NextInterval, 30*time.Minute; got != want {
		t.Errorf("NextInterval = %v, want %v from sy:updatePeriod hourly / frequency 2", got, want)
	}
}

// A response larger than the cap is a failure, not a truncated parse.
func TestOversizedResponseIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>Big</title>`)
		chunk := strings.Repeat("<item><title>x</title></item>", 1000)
		for range 400 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	fake := newFakeStore(testFeed(srv.URL))
	if _, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID); err != nil {
		t.Fatalf("Poll() = %v, want the oversize response recorded as a failure", err)
	}

	if len(fake.failures) != 1 {
		t.Fatalf("recorded %d failures, want 1", len(fake.failures))
	}
	if got := fake.failures[0].Cause; !strings.Contains(got, "limit") {
		t.Errorf("failure cause = %q, want it to mention the size limit", got)
	}
}

// A database failure mid-poll is our problem, not the feed's: it must surface
// as an error rather than being written to the feed as if the site misbehaved.
func TestDatabaseFailureIsReturnedNotRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, rssTwoItems)
	}))
	defer srv.Close()

	fake := newFakeStore(testFeed(srv.URL))
	fake.insertItemAt = 1

	if _, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID); err == nil {
		t.Fatal("Poll() = nil, want the database failure returned")
	}
	if len(fake.failures) != 0 {
		t.Errorf("recorded %d failures on the feed, want 0 — our fault is not the feed's fault", len(fake.failures))
	}
}

func TestPollSendsHonestUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		_, _ = io.WriteString(w, rssTwoItems)
	}))
	defer srv.Close()

	poller := feed.NewPoller(newFakeStore(testFeed(srv.URL)),
		httpclient.New(httpclient.Options{
			UserAgent:   httpclient.UserAgent("1.2.3", "https://example.com/about"),
			MaxAttempts: 1,
		}),
		feed.DefaultIntervalPolicy(), 20, discardLogger())

	if _, err := poller.Poll(t.Context(), testUserID, testFeedID); err != nil {
		t.Fatalf("Poll() = %v", err)
	}

	if want := "tomekeeper/1.2.3 (+https://example.com/about)"; got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
	if strings.Contains(strings.ToLower(got), "mozilla") {
		t.Error("the User-Agent impersonates a browser")
	}
}
