package server_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/server"
	"github.com/runlevel-six/tomekeeper/internal/session"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// feedSite serves a feed, and a page that advertises one, so that testing a feed
// URL can be exercised without reaching the network.
func feedSite(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, `<?xml version="1.0"?>
<rss version="2.0"><channel>
  <title>Example Engineering</title>
  <link>https://engineering.example.com/</link>
  <description>Notes from the engineering team.</description>
  <item><title>Rebuilding the ingest pipeline</title>
    <link>https://engineering.example.com/posts/ingest</link>
    <pubDate>Mon, 11 Aug 2026 09:00:00 GMT</pubDate></item>
  <item><title>What we learned from a bad deploy</title>
    <link>https://engineering.example.com/posts/deploy</link>
    <pubDate>Mon, 04 Aug 2026 09:00:00 GMT</pubDate></item>
</channel></rss>`)
	})

	// A site's front page, which is what people actually have to hand.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><head>
  <title>Example Engineering</title>
  <link rel="alternate" type="application/rss+xml" title="Posts" href="/feed.xml">
</head><body><h1>Example Engineering</h1></body></html>`)
	})

	// A page with no feed at all.
	mux.HandleFunc("/plain", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Nothing here</title></head>
<body><p>No feed is advertised on this page.</p></body></html>`)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fetchingFixture is a signed-in reader whose server can make outbound requests.
func fetchingFixture(t *testing.T) (*reader, twoReadersHTTP) {
	t.Helper()

	tr := setupTwoReadersFor(t)

	sessions, err := session.NewCookie([]byte("add feed test secret"), session.DefaultTTL, true)
	if err != nil {
		t.Fatalf("NewCookie() = %v", err)
	}
	seedPassword(t, tr)

	srv := server.New(testConfig(), discardLogger(), server.Deps{
		Store:    tr.store,
		Sessions: sessions,
		// The whole point of this fixture: the web interface may fetch.
		Fetch: httpclient.New(httpclient.Options{UserAgent: "tomekeeper-test", Concurrency: 2}),
	})

	rd := &reader{t: t, h: srv.Handler(), user: tr.alice}

	login := postLogin(t, rd.h, "tome", testPassword)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("signing in the fixture = %d", login.Code)
	}
	rd.jar = login.Result().Cookies()

	return rd, tr
}

// Testing a feed reports what is there and writes nothing.
func TestAddFeedTestReportsWithoutSubscribing(t *testing.T) {
	rd, tr := fetchingFixture(t)
	site := feedSite(t)

	before := feedCount(t, tr)

	rec := rd.do(http.MethodPost, "/feeds/test", url.Values{"url": {site.URL + "/feed.xml"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /feeds/test = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// What the feed calls itself, how much it carries, and a title a reader would
	// recognize: the three things that answer "is this the feed I meant".
	for _, want := range []string{
		"Example Engineering",
		"2 items",
		"Rebuilding the ingest pipeline",
	} {
		if !strings.Contains(body, want) && !strings.Contains(body, escaped(want)) {
			t.Errorf("the test result does not mention %q:\n%s", want, body)
		}
	}

	// And nothing was subscribed to.
	if after := feedCount(t, tr); after != before {
		t.Errorf("testing a feed created %d subscriptions", after-before)
	}

	// The form comes back filled in, so the next step needs no retyping.
	if !strings.Contains(body, `value="`+site.URL+`/feed.xml"`) {
		t.Errorf("the form lost the URL that was tested:\n%s", body)
	}
}

// A site's address is followed to the feed it advertises, because that is the URL
// people actually have.
func TestAddFeedTestDiscoversTheFeedFromASitePage(t *testing.T) {
	rd, _ := fetchingFixture(t)
	site := feedSite(t)

	rec := rd.do(http.MethodPost, "/feeds/test", url.Values{"url": {site.URL}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /feeds/test = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "advertises") {
		t.Errorf("the page does not say the feed was discovered rather than given:\n%s", body)
	}
	// The discovered URL is what the form now holds, so adding subscribes to the
	// feed rather than to the front page.
	if !strings.Contains(body, `value="`+site.URL+`/feed.xml"`) {
		t.Errorf("the form does not hold the discovered feed URL:\n%s", body)
	}
}

// A page with no feed says so, in terms that name the next thing to try.
func TestAddFeedTestReportsAPageWithNoFeed(t *testing.T) {
	rd, _ := fetchingFixture(t)
	site := feedSite(t)

	rec := rd.do(http.MethodPost, "/feeds/test", url.Values{"url": {site.URL + "/plain"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /feeds/test = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "does not advertise one") {
		t.Errorf("a page with no feed was not explained:\n%s", body)
	}
}

// Adding subscribes, files the feed, and does not need a test first.
func TestAddFeedSubscribes(t *testing.T) {
	rd, tr := fetchingFixture(t)
	site := feedSite(t)

	rec := rd.do(http.MethodPost, "/feeds/add", url.Values{
		"url":      {site.URL + "/feed.xml"},
		"category": {"Engineering"},
		"title":    {"Example Engineering"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /feeds/add = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Subscribed") {
		t.Errorf("the page does not confirm the subscription:\n%s", body)
	}

	added, err := tr.store.FeedByURL(t.Context(), tr.alice, site.URL+"/feed.xml")
	if err != nil {
		t.Fatalf("FeedByURL() = %v", err)
	}
	if added.Title != "Example Engineering" || added.Category != "Engineering" {
		t.Errorf("stored feed = %q in %q, want the submitted title and category",
			added.Title, added.Category)
	}

	// It is in the list, and the category links to that category's stream.
	body := rd.body("/feeds")
	if !strings.Contains(body, "Example Engineering") {
		t.Errorf("the new feed is not in the list:\n%s", body)
	}

	// Adding the same URL again updates rather than duplicating.
	rec = rd.do(http.MethodPost, "/feeds/add", url.Values{
		"url":      {site.URL + "/feed.xml"},
		"category": {"Engineering"},
		"title":    {"Example Engineering, renamed"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /feeds/add twice = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Already subscribed") {
		t.Errorf("a repeated add did not say it was already subscribed:\n%s", body)
	}
	if n := feedCountFor(t, tr, site.URL+"/feed.xml"); n != 1 {
		t.Errorf("the same feed URL exists %d times, want 1", n)
	}
}

// A URL with no scheme is what an address bar shows, so it has to work.
func TestAddFeedAcceptsASchemelessURL(t *testing.T) {
	rd, tr := fetchingFixture(t)

	rec := rd.do(http.MethodPost, "/feeds/add", url.Values{"url": {"example.com/feed.xml"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /feeds/add = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	if _, err := tr.store.FeedByURL(t.Context(), tr.alice, "https://example.com/feed.xml"); err != nil {
		t.Errorf("a schemeless URL was not stored as https: %v", err)
	}
}

// What is not a web address is refused with the form still filled in.
func TestAddFeedRefusesSomethingThatIsNotAURL(t *testing.T) {
	rd, tr := fetchingFixture(t)

	before := feedCount(t, tr)

	rec := rd.do(http.MethodPost, "/feeds/add", url.Values{"url": {"not a url at all"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /feeds/add with rubbish = %d, want 400", rec.Code)
	}
	if after := feedCount(t, tr); after != before {
		t.Error("a rejected address was subscribed to anyway")
	}
}

// Without an outbound client the form still adds; only testing is unavailable, and
// it says so rather than failing obscurely.
func TestAddFeedWithoutAFetchClient(t *testing.T) {
	rd, tr := readingFixture(t)

	if body := rd.body("/feeds"); strings.Contains(body, `formaction="/feeds/test"`) {
		t.Error("the test button is offered by an instance that cannot test")
	}

	rec := rd.do(http.MethodPost, "/feeds/test", url.Values{"url": {"https://example.com/feed.xml"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /feeds/test = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "cannot test a feed") {
		t.Errorf("the page does not explain that testing is unavailable:\n%s", body)
	}

	// Adding still works, which is the point of degrading rather than refusing.
	rec = rd.do(http.MethodPost, "/feeds/add", url.Values{"url": {"https://example.com/feed.xml"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /feeds/add = %d, want 200", rec.Code)
	}
	if _, err := tr.store.FeedByURL(t.Context(), tr.alice, "https://example.com/feed.xml"); err != nil {
		t.Errorf("the feed was not added: %v", err)
	}
}

// The categories that exist are offered to choose from, which is what keeps "Tech"
// and "tech" from both existing — and filing a feed under nothing is a visible
// option rather than a field you have to guess at emptying.
//
// Asserted as properties rather than as markup: this was a text field with a
// datalist and is now a select with a companion field, and a test naming the element
// would have failed for a change that improved the thing it was protecting.
func TestFeedFormOffersTheCategoriesThatExist(t *testing.T) {
	rd, tr := fetchingFixture(t)

	if _, _, err := tr.store.UpsertFeed(t.Context(), tr.alice, store.FeedParams{
		FeedURL: "https://example.com/comics.xml", Title: "Comics", Category: "Comics",
	}); err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	// An empty category has to be offered too, or it can never be filled — which is
	// most of why categories became a table.
	if _, err := tr.store.CreateCategory(t.Context(), tr.alice, "Reading later"); err != nil {
		t.Fatalf("CreateCategory() = %v", err)
	}

	body := rd.body("/feeds")
	for _, want := range []string{
		`<option value="Comics">`,
		`<option value="Reading later">`,
		`name="new_category"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the category control is missing %s:\n%s", want, body)
		}
	}

	// "No category" is a choice and must look like one. Emptying a text field was how
	// this used to be done, and nobody found it.
	if !strings.Contains(body, `<option value="">`) {
		t.Errorf("the form offers no explicit way to file a feed under nothing:\n%s", body)
	}
}

// The two halves of the control resolve to one answer, and a typed name wins: it is
// the more specific act, and the picker always has some value so it would otherwise
// silently overrule somebody who filled the field in.
func TestATypedCategoryBeatsThePicker(t *testing.T) {
	rd, tr := fetchingFixture(t)
	ctx := t.Context()

	if _, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL: "https://example.com/comics.xml", Title: "Comics", Category: "Comics",
	}); err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	feed, err := tr.store.FeedByURL(ctx, tr.alice, "https://example.com/comics.xml")
	if err != nil {
		t.Fatalf("FeedByURL() = %v", err)
	}
	id := strconv.FormatInt(int64(feed.ID), 10)

	// Both filled: the typed one is what happens.
	rec := rd.do(http.MethodPost, "/feeds/"+id+"/edit", url.Values{
		"url": {feed.FeedURL}, "title": {feed.Title},
		"category": {"Comics"}, "new_category": {"Webcomics"},
		"enabled": {"on"}, "poll_every": {""},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST edit = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if feed, err = tr.store.GetFeed(ctx, tr.alice, feed.ID); err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if feed.Category != "Webcomics" {
		t.Errorf("category = %q, want the typed %q to win over the picked %q",
			feed.Category, "Webcomics", "Comics")
	}

	// And the explicit empty option files it under nothing.
	rec = rd.do(http.MethodPost, "/feeds/"+id+"/edit", url.Values{
		"url": {feed.FeedURL}, "title": {feed.Title},
		"category": {""}, "new_category": {""},
		"enabled": {"on"}, "poll_every": {""},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST edit = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if feed, err = tr.store.GetFeed(ctx, tr.alice, feed.ID); err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if feed.Category != "" || feed.CategoryID != 0 {
		t.Errorf("category = %q/%d, want the feed filed under nothing", feed.Category, feed.CategoryID)
	}
}

// One reader's feeds are not another's, through this form as much as anywhere.
func TestAddFeedIsScopedToTheReader(t *testing.T) {
	rd, tr := fetchingFixture(t)

	// Bob's feed URL, added by Alice: she gets her own subscription to the same
	// URL, not a view of his.
	bobFeed, err := tr.store.GetFeed(t.Context(), tr.bob, tr.bobFeed)
	if err != nil {
		t.Fatalf("GetFeed(bob) = %v", err)
	}

	rec := rd.do(http.MethodPost, "/feeds/add", url.Values{
		"url": {bobFeed.FeedURL}, "category": {"Hers"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /feeds/add = %d, want 200", rec.Code)
	}

	hers, err := tr.store.FeedByURL(t.Context(), tr.alice, bobFeed.FeedURL)
	if err != nil {
		t.Fatalf("FeedByURL(alice) = %v", err)
	}
	if hers.ID == tr.bobFeed {
		t.Error("Alice's add returned Bob's feed row")
	}
	if hers.Category != "Hers" {
		t.Errorf("Alice's copy is filed under %q, want Hers", hers.Category)
	}

	his, err := tr.store.GetFeed(t.Context(), tr.bob, tr.bobFeed)
	if err != nil {
		t.Fatalf("GetFeed(bob) = %v", err)
	}
	if his.Category == "Hers" {
		t.Error("Alice's filing changed Bob's category")
	}
}

func feedCount(t *testing.T, tr twoReadersHTTP) int64 {
	t.Helper()

	var n int64
	if err := tr.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM feeds WHERE user_id = $1`, tr.alice).Scan(&n); err != nil {
		t.Fatalf("counting feeds: %v", err)
	}
	return n
}

func feedCountFor(t *testing.T, tr twoReadersHTTP, feedURL string) int64 {
	t.Helper()

	var n int64
	if err := tr.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM feeds WHERE user_id = $1 AND feed_url = $2`,
		tr.alice, feedURL).Scan(&n); err != nil {
		t.Fatalf("counting feeds: %v", err)
	}
	return n
}

// escaped is the HTML-escaped form of a string, for asserting against rendered
// output where a title carries an apostrophe or an ampersand.
func escaped(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "&", "&amp;"), "'", "&#39;")
}
