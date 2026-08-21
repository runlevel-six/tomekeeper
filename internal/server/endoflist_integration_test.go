package server_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// fragment asks for a page of rows the way infinite scroll does, so a test can
// see what a reader scrolling to the bottom is actually sent.
func fragment(t *testing.T, rd *reader, path string) string {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	req.Header.Set("HX-Request", "true")
	for _, c := range rd.jar {
		req.AddCookie(c)
	}

	rec := httptest.NewRecorder()
	rd.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s (htmx) = %d, want 200\n%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// nextPageOf pulls the cursor out of the infinite-scroll attribute on the last
// row, which is the only place it exists — the same string htmx would request.
func nextPageOf(t *testing.T, body string) string {
	t.Helper()

	const marker = `hx-get="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no next page in this render; it already reaches the end")
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("malformed hx-get attribute")
	}
	return strings.ReplaceAll(rest[:j], "&amp;", "&")
}

// markCountIn reads the number out of a "Mark N as read" control.
func markCountIn(t *testing.T, body string) int {
	t.Helper()

	i := strings.Index(body, "stream-end")
	if i < 0 {
		t.Fatalf("no end-of-list control in this render")
	}
	rest := body[i:]
	// The element's text, not its title attribute — which also begins "Mark " and
	// comes first.
	const marker = ">Mark "
	j := strings.Index(rest, marker)
	if j < 0 {
		t.Fatalf("the end-of-list control names no count:\n%s", rest)
	}
	rest = rest[j+len(marker):]
	k := strings.Index(rest, " as read")
	if k < 0 {
		t.Fatalf("malformed mark control:\n%s", rest)
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest[:k]))
	if err != nil {
		t.Fatalf("the count in the end-of-list control is not a number: %v", err)
	}
	return n
}

// seedManyUnread fills the reader's feed with enough articles to page.
func seedManyUnread(t *testing.T, tr twoReadersHTTP, n int) {
	t.Helper()
	ctx := t.Context()

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for i := range n {
		published := base.Add(time.Duration(i) * time.Minute)
		slug := "page-" + strconv.Itoa(i)

		id, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
			URLCanonical: "https://example.com/" + slug,
			Title:        "Paged " + strconv.Itoa(i),
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
	}
}

// The end of a long list has to carry the mark-read control, and the end of a long
// list arrives as an appended fragment rather than as the document — so a control
// rendered only by the document is a control a reader scrolling a real list never
// sees.
//
// This is the whole feature: the control at the top of the page is forty pages away
// by the time somebody finishes, and marking read on the way past cannot reach the
// last screenful, because those rows never leave over the top edge.
func TestTheEndOfAPagedListCarriesTheMarkControl(t *testing.T) {
	rd, tr := readingFixture(t)
	seedManyUnread(t, tr, store.DefaultStreamLimit+5)

	first := rd.body("/")
	if strings.Contains(first, "stream-end") {
		t.Error("the first page of a longer list drew an end-of-list control; it is not the end")
	}

	last := fragment(t, rd, nextPageOf(t, first))
	if !strings.Contains(last, "stream-end") {
		t.Fatalf("the final page carries no end-of-list control:\n%s", last)
	}
	if !strings.Contains(last, `href="/mark-read?from=`) {
		t.Errorf("the end-of-list control does not link to the confirmation:\n%s", last)
	}
	if !strings.Contains(last, "data-pull-to-mark") {
		t.Error("the end-of-list control carries no hook for the pull gesture")
	}

	// The count is what makes the second step of the confirmation an informed one,
	// and it has to be the *list's* count rather than the fragment's. Asserted as
	// "more than one page" rather than an exact number: the exact number depends on
	// what the fixture seeds, while the property under test is that a fragment of 5
	// rows does not report 5.
	marked := markCountIn(t, last)
	if marked <= store.DefaultStreamLimit {
		t.Errorf("the end-of-list control offers to mark %d, which is no more than the %d rows on a page — it counted the fragment, not the list",
			marked, store.DefaultStreamLimit)
	}
}

// A short list reaches its end in the document, and gets the same control there.
func TestAShortListCarriesTheMarkControlAtItsEnd(t *testing.T) {
	rd, tr := readingFixture(t)
	seedManyUnread(t, tr, 3)

	body := rd.body("/")
	if !strings.Contains(body, "stream-end") {
		t.Fatalf("a list that fits on one page has no end-of-list control:\n%s", body)
	}
	// Both controls are the same request. Two ways to reach it, one path to the write.
	if strings.Count(body, `href="/mark-read?from=`) != 2 {
		t.Errorf("want the head control and the end control, both linking to the confirmation; got %d links",
			strings.Count(body, `href="/mark-read?from=`))
	}
}

// The lists that must not be marked in bulk must not grow a second way to do it.
// Search carries an empty query, so applying it would mark the whole archive read
// from a page showing four results — the reason Markable is opt-in at all.
//
// **Starred is the case that tests the guard**, and it took a neuter to find that
// out. It is drawn by the same renderer as the unread stream, so it reaches the end
// of its list like any other and only its Markable flag keeps the control off it.
// The other three are protected by accidents of construction rather than by the
// guard: search and the attention queue have templates of their own that do not
// include these rows at all, and the Saved page shares the rows but never sets
// AtEnd. They are asserted anyway — a reader must not find a bulk mark on any of
// them, whatever is currently preventing it — but a version of this test with only
// those three passed with the guard deleted.
func TestListsThatCannotBeMarkedHaveNoEndControl(t *testing.T) {
	rd, tr := readingFixture(t)
	seedManyUnread(t, tr, 3)

	// Starred has to have something in it, or the list renders its empty
	// placeholder and never reaches the rows this control lives in — which is how
	// the first version of this test passed with the guard deleted.
	if rec := rd.do(http.MethodPost, "/articles/"+strconv.FormatInt(int64(tr.aliceOnly), 10)+"/star",
		url.Values{"on": {"true"}}); rec.Code != http.StatusOK {
		t.Fatalf("starring an article = %d, want 200", rec.Code)
	}
	if body := rd.body("/starred"); !strings.Contains(body, "entry") {
		t.Fatalf("the starred list is still empty, so it draws no rows:\n%s", body)
	}

	for _, path := range []string{
		"/starred",
		"/search?q=" + url.QueryEscape("body"),
		"/attention",
		"/saved",
	} {
		body := rd.body(path)
		if strings.Contains(body, "stream-end") {
			t.Errorf("%s drew an end-of-list mark control on a list that cannot be marked in bulk", path)
		}
	}
}

// Nothing to mark, nothing to offer. A reader who marked everything read on the way
// down should reach a quiet end, not a control that would do nothing.
func TestAFullyReadListOffersNothingAtItsEnd(t *testing.T) {
	rd, tr := readingFixture(t)
	seedManyUnread(t, tr, 3)

	if rec := rd.do(http.MethodPost, "/mark-read", url.Values{"from": {"unread"}}); rec.Code != http.StatusOK {
		t.Fatalf("POST /mark-read = %d, want 200", rec.Code)
	}

	body := rd.body("/all")
	if strings.Contains(body, "stream-end") {
		t.Errorf("a list with nothing unread still offered to mark it read:\n%s", body)
	}
}
