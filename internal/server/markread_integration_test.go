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

// seedInCategory subscribes the fixture's reader to another feed, filed under a
// category, with one article on it.
func seedInCategory(t *testing.T, tr twoReadersHTTP, category, slug string) store.ArticleID {
	t.Helper()
	ctx := t.Context()

	feedID, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL:  "https://example.com/" + slug + "/feed.xml",
		Title:    strings.ToUpper(slug),
		Category: category,
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	published := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	id, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://example.com/" + slug,
		Title:        slug + " post",
		PublishedAt:  &published,
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if _, err := tr.store.InsertContent(ctx, store.ContentParams{
		ArticleID: id, ExtractorName: "trafilatura", ExtractorVersion: "2",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>body of " + slug + "</p>", Text: "body of " + slug, WordCount: 20,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}
	if _, err := tr.store.InsertFeedItem(ctx, tr.alice, store.FeedItemParams{
		FeedID: feedID, ArticleID: id, GUID: "guid-" + slug,
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
	return id
}

var (
	markLinkPattern  = regexp.MustCompile(`href="(/mark-read\?from=[^"]*)"`)
	markTokenPattern = regexp.MustCompile(`name="from" value="([^"]*)"`)
)

// markLink returns the URL a browser would request from the mark-all-read control.
//
// Taken from the rendered page rather than composed here, because the escaping of a
// category name into that href is part of what is being tested: a folder called
// "Long Reads" has to survive the round trip through an attribute and a query
// string, and a hand-written URL in a test would prove only that the store works.
func markLink(t *testing.T, body string) string {
	t.Helper()

	m := markLinkPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the page offers no mark-all-read control:\n%s", body)
	}
	return html.UnescapeString(m[1])
}

// markToken returns the list token the confirmation form would post back.
func markToken(t *testing.T, body string) string {
	t.Helper()

	m := markTokenPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the confirmation carries no list token:\n%s", body)
	}
	return html.UnescapeString(m[1])
}

// The control asks first, and only the answer marks anything.
func TestMarkAllReadAsksBeforeItActs(t *testing.T) {
	rd, tr := readingFixture(t)

	// The offer names the number, so the size of what is about to happen is visible
	// before the first click and not only after it.
	stream := rd.body("/")
	if !strings.Contains(stream, "Mark 1 as read") {
		t.Errorf("the unread stream does not offer to mark its 1 article read:\n%s", stream)
	}

	confirm := rd.body(markLink(t, stream))
	if !strings.Contains(confirm, `action="/mark-read"`) {
		t.Errorf("the confirmation has no form to submit:\n%s", confirm)
	}
	if !strings.Contains(confirm, "Mark 1 article in") {
		t.Errorf("the confirmation does not say what it would mark:\n%s", confirm)
	}

	// Asking is a GET and changes nothing. This is the assertion that makes the
	// two-step control worth having at all.
	view, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if view.Read {
		t.Fatal("merely asking to mark the list read marked it read")
	}

	rec := rd.do(http.MethodPost, "/mark-read", url.Values{"from": {markToken(t, confirm)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /mark-read = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	marked := rec.Body.String()
	if !strings.Contains(marked, "Marked 1 article read.") {
		t.Errorf("the page does not report what was marked:\n%s", marked)
	}

	view, err = tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if !view.Read {
		t.Error("the article is still unread after the list was marked read")
	}

	// The list it leaves behind no longer offers the control, and no longer counts
	// the article in the chrome — a badge that still says 1 over an empty list is a
	// lie the reader cannot dismiss.
	if strings.Contains(marked, "/mark-read?from=") {
		t.Error("the page still offers to mark a list with nothing unread in it")
	}
	if strings.Contains(marked, `data-unread="1"`) {
		t.Error("the unread count still counts the article that was just marked read")
	}

	// Posting again is harmless and says so, which is what makes rendering the
	// result rather than redirecting safe.
	again := rd.do(http.MethodPost, "/mark-read", url.Values{"from": {"unread"}})
	if again.Code != http.StatusOK {
		t.Fatalf("POST /mark-read twice = %d, want 200", again.Code)
	}
	if !strings.Contains(again.Body.String(), "Nothing here was unread.") {
		t.Errorf("a repeated mark does not report itself honestly:\n%s", again.Body.String())
	}
}

// Marking one list read marks that list, through the whole round trip.
//
// The category name has a space in it on purpose. The token travels in a query
// string and again in a form field, so an escaping mistake anywhere along that path
// would produce a request naming a category nobody has — which fails silently as
// "the button does nothing" rather than as an error.
func TestMarkAllReadIsScopedToTheListOnScreen(t *testing.T) {
	rd, tr := readingFixture(t)

	long := seedInCategory(t, tr, "Long Reads", "essays")
	comic := seedInCategory(t, tr, "Comics", "strips")

	stream := rd.body("/categories?name=Long+Reads")
	if !strings.Contains(stream, "Mark 1 as read") {
		t.Fatalf("the category page does not offer to mark its article read:\n%s", stream)
	}

	confirm := rd.body(markLink(t, stream))
	if !strings.Contains(confirm, "Long Reads") {
		t.Errorf("the confirmation does not name the category it would mark:\n%s", confirm)
	}

	rec := rd.do(http.MethodPost, "/mark-read", url.Values{"from": {markToken(t, confirm)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /mark-read = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Marked 1 article read.") {
		t.Errorf("the category mark reported nothing:\n%s", rec.Body.String())
	}

	for _, want := range []struct {
		id   store.ArticleID
		read bool
		what string
	}{
		{long, true, "the article in Long Reads"},
		{comic, false, "the article in Comics"},
		{tr.aliceOnly, false, "the article in no category at all"},
	} {
		view, err := tr.store.ArticleForUser(t.Context(), tr.alice, want.id)
		if err != nil {
			t.Fatalf("ArticleForUser(%d) = %v", want.id, err)
		}
		if view.Read != want.read {
			t.Errorf("%s: read = %v, want %v", want.what, view.Read, want.read)
		}
	}
}

// A list whose query does not describe its contents cannot be marked read, and
// neither can somebody else's.
//
// Search is the one that matters: its spec carries an empty StreamQuery because the
// results come from a ranked text query the store would have to re-run, so applying
// that query to a bulk mark would mark the reader's entire archive read from a page
// showing four results. The attention queue is the same shape. Both are refused
// here rather than being trusted not to be asked for.
func TestMarkAllReadRefusesAListItCannotScope(t *testing.T) {
	rd, tr := readingFixture(t)

	bobFeed := "feed:" + strconv.FormatInt(int64(tr.bobFeed), 10)

	for _, token := range []string{
		"search:alpaca", // ranked, not filtered — the dangerous one
		"attention",     // a worklist, selected by fetch status
		"starred",       // hand-picked, one article at a time
		"saved",
		"nonsense",
		"feed:0",
		"feed:99999",
		bobFeed, // another reader's feed, which is not found rather than forbidden
	} {
		t.Run(token, func(t *testing.T) {
			get := rd.get("/mark-read?from=" + url.QueryEscape(token))
			if get.Code != http.StatusNotFound {
				t.Errorf("GET /mark-read?from=%s = %d, want 404", token, get.Code)
			}
			post := rd.do(http.MethodPost, "/mark-read", url.Values{"from": {token}})
			if post.Code != http.StatusNotFound {
				t.Errorf("POST /mark-read from=%s = %d, want 404", token, post.Code)
			}
		})
	}

	// A missing token is refused too, rather than defaulting to some list.
	if rec := rd.get("/mark-read"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /mark-read with no list = %d, want 404", rec.Code)
	}
	if rec := rd.do(http.MethodPost, "/mark-read", url.Values{}); rec.Code != http.StatusNotFound {
		t.Errorf("POST /mark-read with no list = %d, want 404", rec.Code)
	}

	// And nothing was marked along the way — a refused request that had already
	// written the rows would be the worst of both.
	view, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if view.Read {
		t.Error("a refused mark-read request marked the archive read anyway")
	}

	// The pages that cannot be marked do not pretend otherwise.
	for _, path := range []string{"/search?q=alpaca", "/attention", "/starred", "/saved"} {
		if body := rd.body(path); strings.Contains(body, "/mark-read") {
			t.Errorf("%s offers a control it would refuse", path)
		}
	}
}

// The chrome's reload control survives a mark.
//
// pageData takes the reload link from the request's own path, and a mark is posted
// to /mark-read — so without care the page rendered afterwards offers a reload
// button pointing at a bare /mark-read, which is a 404. Installed as a web app
// that button is the only reload there is.
func TestMarkAllReadLeavesTheReloadControlPointingAtTheList(t *testing.T) {
	rd, _ := readingFixture(t)

	for _, page := range []struct {
		name string
		body func() string
	}{
		{"the confirmation", func() string { return rd.body("/mark-read?from=unread") }},
		{"the page after marking", func() string {
			rec := rd.do(http.MethodPost, "/mark-read", url.Values{"from": {"unread"}})
			if rec.Code != http.StatusOK {
				t.Fatalf("POST /mark-read = %d, want 200", rec.Code)
			}
			return rec.Body.String()
		}},
	} {
		t.Run(page.name, func(t *testing.T) {
			body := page.body()
			if !strings.Contains(body, `class="tool reload" href="/"`) {
				t.Errorf("%s does not point the reload control at the unread list:\n%s", page.name, body)
			}
		})
	}
}
