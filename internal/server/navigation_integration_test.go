package server_test

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// These cover the way *out* of a page, which an installed web app has no browser
// chrome to provide. Every one of them is a control that does not exist anywhere
// else: there is no back button, no address bar and no reload behind a standalone
// window, so if the page does not draw it the reader has no way to it at all.

var (
	backHref = regexp.MustCompile(`<a class="back" href="([^"]+)"`)
	relHref  = regexp.MustCompile(`href="([^"]+)"[^>]*rel="(prev|next)"`)
)

// seedRun puts three articles on Alice's feed, published a minute apart, and
// returns them newest-first.
func seedRun(t *testing.T, tr twoReadersHTTP) []store.ArticleID {
	t.Helper()
	ctx := t.Context()

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	var newest []store.ArticleID

	for i := range 3 {
		published := base.Add(time.Duration(i) * time.Minute)
		slug := "run-" + strconv.Itoa(i)

		id, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
			URLCanonical: "https://example.com/" + slug,
			Title:        "Run " + strconv.Itoa(i),
			PublishedAt:  &published,
		})
		if err != nil {
			t.Fatalf("UpsertArticle() = %v", err)
		}
		if _, err := tr.store.InsertContent(ctx, store.ContentParams{
			ArticleID: id, ExtractorName: "trafilatura", ExtractorVersion: "2",
			ContentOrigin: store.OriginFetched,
			HTML:          "<p>body</p>", Text: "body", WordCount: 20,
		}); err != nil {
			t.Fatalf("InsertContent() = %v", err)
		}
		if _, err := tr.store.InsertFeedItem(ctx, tr.alice, store.FeedItemParams{
			FeedID: tr.aliceFeed, ArticleID: id, GUID: "guid-" + slug,
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}
		newest = append([]store.ArticleID{id}, newest...)
	}
	return newest
}

func siblings(t *testing.T, body string) map[string]string {
	t.Helper()

	out := map[string]string{}
	for _, m := range relHref.FindAllStringSubmatch(body, -1) {
		out[m[2]] = strings.ReplaceAll(m[1], "&amp;", "&")
	}
	return out
}

// The article page must offer a way back to the list it was opened from, and it
// must be the list rather than a guess.
func TestArticlePageLinksBackToTheListItCameFrom(t *testing.T) {
	rd, tr := readingFixture(t)

	for _, c := range []struct{ from, wantPath, wantLabel string }{
		{"unread", "/", "Unread"},
		{"all", "/all", "Everything"},
		{"starred", "/starred", "Starred"},
		{"saved", "/saved", "Saved"},
		{"attention", "/attention", "Attention"},
		{"feed:" + strconv.FormatInt(int64(tr.aliceFeed), 10), "/feeds/" +
			strconv.FormatInt(int64(tr.aliceFeed), 10), "Alice&#39;s Feed"},
	} {
		path := "/articles/" + strconv.FormatInt(int64(tr.aliceOnly), 10) +
			"?from=" + url.QueryEscape(c.from)
		body := rd.body(path)

		m := backHref.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("from=%s: the article page has no back link:\n%s", c.from, body)
		}
		if got := strings.ReplaceAll(m[1], "&amp;", "&"); got != c.wantPath {
			t.Errorf("from=%s: back link = %q, want %q", c.from, got, c.wantPath)
		}
		if !strings.Contains(body, c.wantLabel) {
			t.Errorf("from=%s: the back link is not labeled %q", c.from, c.wantLabel)
		}
	}
}

// A bare link, or a tampered one, still leaves a way out. Falling through to
// nothing would strand a reader on a page with no exit.
func TestArticlePageAlwaysHasAWayOut(t *testing.T) {
	rd, tr := readingFixture(t)
	id := strconv.FormatInt(int64(tr.aliceOnly), 10)

	for _, from := range []string{
		"",                  // a bare link
		"?from=",            // present but empty
		"?from=nonsense",    // not a list
		"?from=feed:999999", // not one of hers
		"?from=feed:abc",    // not a number
		"?from=tag:0",       // not a valid id
	} {
		body := rd.body("/articles/" + id + from)

		m := backHref.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%q: no back link at all", from)
		}
		if got := m[1]; got != "/" {
			t.Errorf("%q: back link = %q, want the unread list", from, got)
		}
		// An unrecognized list grants no previous/next, because there is no list
		// to be previous or next within.
		if got := siblings(t, body); len(got) != 0 {
			t.Errorf("%q: offered previous/next %v with no list", from, got)
		}
	}
}

// Next and previous must land on what the list showed either side. This is the
// assertion that would catch the article page and the stream disagreeing about
// what "the unread list" contains.
func TestPreviousAndNextFollowTheList(t *testing.T) {
	rd, tr := readingFixture(t)
	run := seedRun(t, tr) // newest first

	middle := strconv.FormatInt(int64(run[1]), 10)
	body := rd.body("/articles/" + middle + "?from=all")

	got := siblings(t, body)
	wantPrev := "/articles/" + strconv.FormatInt(int64(run[0]), 10) + "?from=all"
	wantNext := "/articles/" + strconv.FormatInt(int64(run[2]), 10) + "?from=all"

	if got["prev"] != wantPrev {
		t.Errorf("previous = %q, want %q", got["prev"], wantPrev)
	}
	if got["next"] != wantNext {
		t.Errorf("next = %q, want %q", got["next"], wantNext)
	}
}

// Search results are ranked rather than ordered, so they get a way back but no
// previous/next: "the next best match" is not a reading order.
func TestSearchResultsGiveAWayBackButNoReadingOrder(t *testing.T) {
	rd, tr := readingFixture(t)
	_ = tr

	body := rd.body("/articles/" + strconv.FormatInt(int64(tr.aliceOnly), 10) +
		"?from=" + url.QueryEscape("search:alpaca"))

	m := backHref.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no back link from a search result")
	}
	if got := strings.ReplaceAll(m[1], "&amp;", "&"); got != "/search?q=alpaca" {
		t.Errorf("back link = %q, want the search that found it", got)
	}
	if got := siblings(t, body); len(got) != 0 {
		t.Errorf("search results offered previous/next %v", got)
	}
}

// The category index lists the folders the feeds are filed under, and each one
// leads to a stream of its articles.
func TestCategoryIndexAndStream(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	// File Alice's feed under a name with a space and an ampersand in it, which is
	// exactly what breaks a category carried in a path segment.
	const category = "Comics & Strips"
	if _, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL: "https://alice.example.com/f.xml", Title: "Alice's Feed", Category: category,
	}); err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	index := rd.body("/categories")
	if !strings.Contains(index, "Comics &amp; Strips") {
		t.Errorf("the category index does not list %q:\n%s", category, index)
	}

	// Follow the link the page actually drew rather than asserting how it was
	// escaped. Escaping is the template's business and there is more than one
	// correct answer — a space may be `+` or `%20`, and either may then be
	// HTML-escaped — but there is only one correct *destination*.
	// `?name=` specifically. The index also carries `?edit=` and `?delete=` links
	// now, and matching any /categories? link took the last one on the page — which
	// became a management action rather than a stream, and failed with a message
	// about a missing article.
	categoryLink := ""
	for _, m := range regexp.MustCompile(`href="(/categories\?name=[^"]*)"`).FindAllStringSubmatch(index, -1) {
		categoryLink = html.UnescapeString(m[1])
	}
	if categoryLink == "" {
		t.Fatalf("the category index has no link to a category:\n%s", index)
	}

	stream := rd.body(categoryLink)
	if !strings.Contains(stream, "Alice&#39;s Article") && !strings.Contains(stream, "Alice's Article") {
		t.Errorf("GET %s does not list the article:\n%s", categoryLink, stream)
	}
	if strings.Contains(stream, "Bob&#39;s Article") || strings.Contains(stream, "Bob's Article") {
		t.Error("the category stream lists the other reader's article")
	}

	// The whole round trip: open an article from this stream and the way back must
	// return to this same stream, category name and all. A name with a space and an
	// ampersand in it is what breaks a token that is not escaped at every hop.
	m := articleHref.FindStringSubmatch(stream)
	if m == nil {
		t.Fatalf("the category stream has no article link:\n%s", stream)
	}
	articleLink := html.UnescapeString(regexp.MustCompile(`href="([^"]+)"`).FindStringSubmatch(m[0])[1])

	article := rd.body(articleLink)
	back := backHref.FindStringSubmatch(article)
	if back == nil {
		t.Fatalf("GET %s has no back link:\n%s", articleLink, article)
	}
	if got := html.UnescapeString(back[1]); got != categoryLink {
		t.Errorf("back from an article opened in %q = %q, want %q", category, got, categoryLink)
	}
	if !strings.Contains(article, category) &&
		!strings.Contains(article, "Comics &amp; Strips") {
		t.Errorf("the back link is not labeled with the category:\n%s", article)
	}
}

// Paging a category, which is the one list whose URL already carries a query
// parameter. Appending "?before=…" to it unconditionally would produce two
// question marks and a next-page link that silently pages from the beginning —
// which reads as "the list just repeats itself" rather than as a broken URL.
func TestCategoryStreamPages(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	const category = "Comics & Strips"
	if _, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL: "https://alice.example.com/f.xml", Title: "Alice's Feed", Category: category,
	}); err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	// Enough to need a second page, so the assertion is on the link the
	// application generated rather than on one the test built.
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for i := range store.DefaultStreamLimit + 5 {
		slug := "cat-" + strconv.Itoa(i)
		published := base.Add(time.Duration(i) * time.Minute)

		id, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
			URLCanonical: "https://example.com/" + slug,
			Title:        "Filed " + strconv.Itoa(i),
			PublishedAt:  &published,
		})
		if err != nil {
			t.Fatalf("UpsertArticle() = %v", err)
		}
		if _, err := tr.store.InsertFeedItem(ctx, tr.alice, store.FeedItemParams{
			FeedID: tr.aliceFeed, ArticleID: id, GUID: "guid-" + slug,
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}
	}

	seen := map[string]int{}
	path := "/categories?name=" + url.QueryEscape(category)

	for page := 0; page < 5; page++ {
		body := rd.body(path)
		for _, id := range articleLinks(body) {
			seen[id]++
		}

		next := nextPageLink(body)
		if next == "" {
			break
		}

		// The composed URL keeps the category and gains exactly one separator.
		if n := strings.Count(next, "?"); n != 1 {
			t.Fatalf("next-page link has %d question marks, want 1: %s", n, next)
		}
		if !strings.Contains(next, url.QueryEscape(category)) &&
			!strings.Contains(next, "Comics+%26+Strips") {
			t.Fatalf("next-page link lost the category: %s", next)
		}
		path = next
	}

	// Every article on the feed, once. Alice's fixture article is on the same feed,
	// so it counts; Bob's is not visible and must not appear.
	want := store.DefaultStreamLimit + 5 + 1
	if len(seen) != want {
		t.Errorf("paging a category saw %d distinct articles, want %d", len(seen), want)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("article %s appeared %d times across pages", id, n)
		}
	}
}

// The unread count leads the page title, because a browser tab or an app switcher
// is the only place an installed app can say anything from behind its own icon.
func TestUnreadCountLeadsThePageTitle(t *testing.T) {
	rd, _ := readingFixture(t)

	body := rd.body("/")
	if !strings.Contains(body, "<title>(1) Unread — Tomekeeper</title>") {
		t.Errorf("the title does not lead with the unread count:\n%s",
			body[:min(len(body), 900)])
	}
	if !strings.Contains(body, `data-unread="1"`) {
		t.Error("the body does not carry the count for the app badge")
	}
}

// The sign-in page is rendered from a different type, which is exactly how a
// field the shared layout reads gets forgotten — and the symptom is a 500 on the
// one page an unauthenticated visitor can reach.
func TestSignInPageRendersWithNoReader(t *testing.T) {
	rd, _ := readingFixture(t)

	// No cookie: signed in, the form redirects instead of rendering, and this test
	// is about what an anonymous visitor sees.
	bare := &reader{t: t, h: rd.h}
	rec := bare.do(http.MethodGet, "/login", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<title>Sign in — Tomekeeper</title>") {
		t.Errorf("the sign-in title is wrong, or a shared field is missing:\n%s", body)
	}
	// No count, because there is no reader to have one.
	if strings.Contains(body, "data-unread") {
		t.Error("the sign-in page carries an unread count")
	}
	if !strings.Contains(body, `src="/static/wordmark.png"`) {
		t.Error("the sign-in page does not show the logo")
	}
}

// The reload control is a link to the page itself, and must not be able to become
// a link off the site.
func TestReloadControlPointsAtThisPage(t *testing.T) {
	rd, _ := readingFixture(t)

	body := rd.body("/all?before=123-4")
	if !strings.Contains(body, `class="tool reload" href="/all?before=123-4"`) {
		t.Errorf("the reload control does not point at this page:\n%s",
			body[:min(len(body), 1500)])
	}
}

// The feed refresh reports what it did, and does not pretend to have fetched
// anything.
func TestRefreshFeedsReportsWhatItQueued(t *testing.T) {
	rd, _ := readingFixture(t)

	rec := rd.do(http.MethodPost, "/feeds/refresh", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /feeds/refresh = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "1 feed queued") {
		t.Errorf("the refresh does not say what it queued:\n%s", body)
	}
	if !strings.Contains(body, "within a minute") {
		t.Error("the refresh does not say when the worker will get to it")
	}
}
