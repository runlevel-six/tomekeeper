package server_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// enableScrollMarking turns the preference on the way a reader would: by posting the
// settings form. Not by writing the column, so the form and the behavior cannot
// drift apart — a checkbox whose name the handler does not read would still pass a
// test that set the column itself.
func enableScrollMarking(t *testing.T, rd *reader) {
	t.Helper()

	rec := rd.do(http.MethodPost, "/settings", url.Values{
		"palette":        {""},
		"mode":           {""},
		"mark_on_scroll": {"on"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings = %d\n%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `name="mark_on_scroll" checked`) {
		t.Errorf("the settings page does not show the preference as on:\n%s", firstLines(body))
	}
}

// scrolledPast reports rows as gone past, the way the page's script does.
func scrolledPast(rd *reader, ids ...string) *httptest.ResponseRecorder {
	return rd.do(http.MethodPost, "/mark-read/scrolled",
		url.Values{"ids": {strings.Join(ids, ",")}})
}

// idString renders an article id the way the script would put it in a request.
func idString(id store.ArticleID) string { return strconv.FormatInt(int64(id), 10) }

// Off until it is asked for, and refused rather than silently ignored.
//
// Both halves matter. The attribute is absent, so the script does nothing on a page
// that never asked for it — and the endpoint refuses anyway, which is what makes a
// tab left open from before the preference was turned off stop marking things.
func TestScrollMarkingIsOffUntilTheReaderAsks(t *testing.T) {
	rd, tr := readingFixture(t)

	if body := rd.body("/"); strings.Contains(body, "data-mark-on-scroll") {
		t.Errorf("the unread list offers scroll marking without being asked:\n%s", firstLines(body))
	}

	if rec := scrolledPast(rd, idString(tr.aliceOnly)); rec.Code != http.StatusConflict {
		t.Errorf("POST /mark-read/scrolled with the preference off = %d, want 409", rec.Code)
	}

	view, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if view.Read {
		t.Error("an article was marked read while the preference was off")
	}
}

// Turned on, the unread lists carry it and the rest of the interface does not.
//
// The scope decision, asserted where it can be seen: Everything, a category, a feed
// and a tag are where a reader goes to *find* an article, and scrolling through them
// must not mark the archive read on the way past.
func TestScrollMarkingIsOfferedOnlyOnTheUnreadLists(t *testing.T) {
	rd, tr := readingFixture(t)
	enableScrollMarking(t, rd)

	seedInCategory(t, tr, "Comics", "xkcd")

	on := []string{"/", "/?category=Comics"}
	off := []string{"/all", "/starred", "/saved", "/categories?name=Comics",
		"/feeds/" + strconv.FormatInt(int64(tr.aliceFeed), 10)}

	for _, path := range on {
		if body := rd.body(path); !strings.Contains(body, `data-mark-on-scroll="1"`) {
			t.Errorf("%s does not offer scroll marking:\n%s", path, firstLines(body))
		}
	}
	for _, path := range off {
		if body := rd.body(path); strings.Contains(body, "data-mark-on-scroll") {
			t.Errorf("%s offers scroll marking, which would mark the archive read while browsing it:\n%s",
				path, firstLines(body))
		}
	}
}

// A batch marks its rows and comes back with their controls redrawn.
//
// The response is the contract: out-of-band fragments for exactly the rows that
// changed, so the buttons a reader is looking at agree with the database without the
// script knowing how to draw a button.
func TestScrolledRowsAreMarkedAndTheirControlsRedrawn(t *testing.T) {
	rd, tr := readingFixture(t)
	enableScrollMarking(t, rd)

	first := seedInCategory(t, tr, "Comics", "xkcd")
	second := seedInCategory(t, tr, "Comics", "monkeyuser")

	rec := scrolledPast(rd, idString(first), idString(second))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /mark-read/scrolled = %d\n%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Errorf("the response carries no out-of-band swap, so the controls stay wrong on screen:\n%s", body)
	}
	for _, id := range []store.ArticleID{first, second} {
		if !strings.Contains(body, `id="actions-`+idString(id)+`"`) {
			t.Errorf("no redrawn controls for article %d:\n%s", id, body)
		}
	}
	// The state the fragment reports, rather than merely that a fragment arrived.
	if strings.Count(body, `aria-pressed="true"`) < 2 {
		t.Errorf("the redrawn controls do not report the articles as read:\n%s", body)
	}

	for _, id := range []store.ArticleID{first, second} {
		view, err := tr.store.ArticleForUser(t.Context(), tr.alice, id)
		if err != nil {
			t.Fatalf("ArticleForUser(%d) = %v", id, err)
		}
		if !view.Read {
			t.Errorf("article %d was not marked read", id)
		}
	}

	// Reporting the same rows again writes nothing and therefore redraws nothing,
	// which is what keeps scrolling back and forth over read rows free.
	again := scrolledPast(rd, idString(first))
	if again.Code != http.StatusOK {
		t.Fatalf("POST /mark-read/scrolled twice = %d", again.Code)
	}
	if strings.TrimSpace(again.Body.String()) != "" {
		t.Errorf("a repeated report redrew something:\n%s", again.Body.String())
	}
}

// A starred row scrolls past without being marked, and says so by absence.
//
// The store refuses it; what this pins is the HTTP behavior a reader would notice —
// no fragment comes back for that row, so its control keeps the state it really has
// rather than being redrawn as read.
func TestScrollingPastAStarredArticleLeavesItAlone(t *testing.T) {
	rd, tr := readingFixture(t)
	enableScrollMarking(t, rd)

	ordinary := seedInCategory(t, tr, "Comics", "xkcd")
	starred := seedInCategory(t, tr, "Comics", "monkeyuser")

	if _, err := tr.store.SetStarred(t.Context(), tr.alice, starred, true); err != nil {
		t.Fatalf("SetStarred() = %v", err)
	}

	rec := scrolledPast(rd, idString(ordinary), idString(starred))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /mark-read/scrolled = %d\n%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, `id="actions-`+idString(ordinary)+`"`) {
		t.Errorf("the ordinary article was not marked:\n%s", body)
	}
	if strings.Contains(body, `id="actions-`+idString(starred)+`"`) {
		t.Errorf("the starred article's controls were redrawn, so it was marked read:\n%s", body)
	}

	view, err := tr.store.ArticleForUser(t.Context(), tr.alice, starred)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if view.Read {
		t.Error("a starred article was marked read by scrolling past it")
	}
}

// Another reader's article is not marked, and the response says nothing about it.
//
// The ids come from a page, so this is the boundary that stops a hand-made request
// from confirming what somebody else has archived: an id that is not yours produces
// exactly what a nonexistent one does.
func TestScrolledIDsCannotReachAnotherReader(t *testing.T) {
	rd, tr := readingFixture(t)
	enableScrollMarking(t, rd)

	rec := scrolledPast(rd, idString(tr.bobOnly))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /mark-read/scrolled = %d\n%s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Errorf("the response mentions another reader's article:\n%s", rec.Body.String())
	}

	var rows int
	if err := tr.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM article_state WHERE article_id = $1 AND user_id = $2`,
		tr.bobOnly, tr.alice).Scan(&rows); err != nil {
		t.Fatalf("counting state rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d state rows were written against another reader's article, want 0", rows)
	}
}

// Junk and floods are refused whole rather than partly applied.
//
// A script is the only thing that posts here, so a malformed list is a bug — and the
// symptom of a bug should not be "some of your articles were marked read".
func TestScrolledIDsAreRefusedWholeWhenTheyMakeNoSense(t *testing.T) {
	rd, tr := readingFixture(t)
	enableScrollMarking(t, rd)

	article := seedInCategory(t, tr, "Comics", "xkcd")

	tooMany := make([]string, 0, 201)
	for i := 1; i <= 201; i++ {
		tooMany = append(tooMany, strconv.Itoa(i))
	}

	for name, ids := range map[string]string{
		"a word":         idString(article) + ",nonsense",
		"a negative id":  idString(article) + ",-4",
		"an empty field": idString(article) + ",,7",
		"a flood":        strings.Join(tooMany, ","),
	} {
		rec := rd.do(http.MethodPost, "/mark-read/scrolled", url.Values{"ids": {ids}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST /mark-read/scrolled with %s = %d, want 400", name, rec.Code)
		}
	}

	view, err := tr.store.ArticleForUser(t.Context(), tr.alice, article)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if view.Read {
		t.Error("a refused request still marked the article it named first")
	}
}
