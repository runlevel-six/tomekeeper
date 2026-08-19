package server_test

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// Narrowing the unread list to one category. The token grammar and the control's
// destinations are covered as functions in streams_test.go; these are the things only
// a live page can show — which articles the SQL actually selects, that a bulk mark
// scoped to a category leaves the rest alone, and that an article opened from the
// narrowed list still knows the list it came from.

// filedFixture gives Alice two categories and one unfiled feed, each with an unread
// and a read article, and returns the article ids by title.
func filedFixture(t *testing.T) (*reader, twoReadersHTTP, map[string]store.ArticleID) {
	t.Helper()

	rd, tr := readingFixture(t)
	ctx := t.Context()
	ids := map[string]store.ArticleID{}

	published := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)

	for _, f := range []struct{ title, url, category string }{
		{"Comic Strip", "https://comics.example.com/strip.xml", "Comics"},
		{"Kernel Notes", "https://tech.example.com/kernel.xml", "Tech"},
		{"Loose Wire", "https://wire.example.com/feed.xml", ""},
	} {
		feedID, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
			FeedURL: f.url, Title: f.title, Category: f.category,
		})
		if err != nil {
			t.Fatalf("UpsertFeed(%s) = %v", f.title, err)
		}

		for _, state := range []string{"unread", "read"} {
			title := f.title + " " + state
			published = published.Add(time.Minute)

			id, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
				URLCanonical: "https://example.com/" + strings.ReplaceAll(strings.ToLower(title), " ", "-"),
				Title:        title,
				PublishedAt:  &published,
			})
			if err != nil {
				t.Fatalf("UpsertArticle(%s) = %v", title, err)
			}
			if _, err := tr.store.InsertContent(ctx, store.ContentParams{
				ArticleID: id, ExtractorName: "trafilatura", ExtractorVersion: "2",
				ContentOrigin: store.OriginFetched,
				HTML:          "<p>body</p>", Text: "body", WordCount: 20,
			}); err != nil {
				t.Fatalf("InsertContent() = %v", err)
			}
			if _, err := tr.store.InsertFeedItem(ctx, tr.alice, store.FeedItemParams{
				FeedID: feedID, ArticleID: id, GUID: "guid-" + title,
			}); err != nil {
				t.Fatalf("InsertFeedItem() = %v", err)
			}
			if state == "read" {
				if _, err := tr.store.SetRead(ctx, tr.alice, id, true); err != nil {
					t.Fatalf("SetRead() = %v", err)
				}
			}
			ids[title] = id
		}
	}

	return rd, tr, ids
}

func TestUnreadNarrowedToOneCategory(t *testing.T) {
	rd, _, _ := filedFixture(t)

	body := rd.body("/?category=Comics")

	if !strings.Contains(body, "Comic Strip unread") {
		t.Errorf("the narrowed list is missing its own unread article:\n%s", body)
	}
	// Read articles from the same category are still read...
	if strings.Contains(body, "Comic Strip read") {
		t.Error("the narrowed list includes a read article")
	}
	// ...and unread articles from elsewhere are still elsewhere.
	for _, other := range []string{"Kernel Notes unread", "Loose Wire unread"} {
		if strings.Contains(body, other) {
			t.Errorf("the narrowed list includes %q from another category", other)
		}
	}
	// It says where it is, and that reads as a place because it is also the label on
	// the way back from an article.
	if !strings.Contains(body, "Unread in Comics") {
		t.Errorf("the page does not name the list:\n%s", body)
	}
}

// Present-but-empty selects the feeds carrying no category, which is a real bucket and
// not the same as "do not filter" — the distinction a single dropdown value could not
// have made.
func TestUnreadNarrowedToTheNamelessCategory(t *testing.T) {
	rd, _, _ := filedFixture(t)

	body := rd.body("/?category=")

	if !strings.Contains(body, "Loose Wire unread") {
		t.Errorf("the nameless category does not include its unfiled feed:\n%s", body)
	}
	for _, other := range []string{"Comic Strip unread", "Kernel Notes unread"} {
		if strings.Contains(body, other) {
			t.Errorf("the nameless category includes %q, which is filed", other)
		}
	}
	if !strings.Contains(body, "Unread with no category") {
		t.Errorf("the page does not name the list:\n%s", body)
	}

	// And the unfiltered list is still the unfiltered list.
	if all := rd.body("/"); !strings.Contains(all, "Comic Strip unread") {
		t.Error("narrowing leaked into the plain unread list")
	}
}

// The control is navigation, so it has to say where it goes and where the reader is.
func TestTheCategoryControlIsDrawnWhereItBelongs(t *testing.T) {
	rd, _, _ := filedFixture(t)

	unread := rd.body("/?category=Comics")
	for _, want := range []string{
		`href="/?category=Tech"`, // sideways, staying unread
		`href="/?category="`,     // the nameless bucket
		// The way out, matched with its class so the brand link in the chrome — which
		// is also href="/" — cannot satisfy this on the control's behalf.
		`class="pill" href="/"`,
		`class="pill is-current"`, // you are here, visibly
		`aria-current="page"`,     // and to a screen reader
		">No category</a>",        // labeled, not blank
	} {
		if !strings.Contains(unread, want) {
			t.Errorf("the control on the narrowed unread list lacks %s:\n%s", want, unread)
		}
	}

	// From Everything, narrowing points at the category's own address rather than at a
	// second address for the same articles.
	all := rd.body("/all")
	if !strings.Contains(all, `href="/categories?name=Comics"`) {
		t.Errorf("Everything's control does not point at the category view:\n%s", all)
	}
	if strings.Contains(all, `href="/all?category=`) {
		t.Error("Everything's control invents a second address for a category")
	}

	// The lists where narrowing is meaningless do not carry it.
	for _, path := range []string{"/starred", "/saved"} {
		if body := rd.body(path); strings.Contains(body, `class="stream-filter"`) {
			t.Errorf("%s carries a category control", path)
		}
	}
}

// A hand-typed ?category= on Everything is the same list as the category view, so it
// is sent to that address rather than quietly ignored.
func TestEverythingRedirectsToTheCategoryView(t *testing.T) {
	rd, _, _ := filedFixture(t)

	rec := rd.get("/all?category=Comics")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET /all?category=Comics = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/categories?name=Comics" {
		t.Errorf("redirected to %q", got)
	}
}

// The reason the narrowed list is worth having as a list rather than a filter: it can
// be marked read on its own.
func TestMarkingTheNarrowedListReadLeavesTheRestUnread(t *testing.T) {
	rd, tr, ids := filedFixture(t)
	token := "unread-category:Comics"

	// The control is offered, counted over the whole narrowed list.
	body := rd.body("/?category=Comics")
	if !strings.Contains(body, "Mark 1 as read") {
		t.Errorf("the narrowed list offers no scoped mark-read control:\n%s", body)
	}
	// Percent-encoded, because the token goes into a query string and a folder name
	// can contain the characters that would end it. That escaping is html/template's
	// doing and it is what keeps a category called "Q&A" from truncating the link.
	if !strings.Contains(strings.ToLower(body), "from=unread-category%3acomics") {
		t.Errorf("the control does not name the narrowed list:\n%s", body)
	}

	// The confirmation names the list rather than "Unread".
	confirm := rd.body("/mark-read?from=" + url.QueryEscape(token))
	if !strings.Contains(confirm, "Unread in Comics") {
		t.Errorf("the confirmation does not name the narrowed list:\n%s", confirm)
	}

	rec := rd.do(http.MethodPost, "/mark-read", url.Values{"from": {token}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /mark-read = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Marked 1 article read") {
		t.Errorf("the mark did not report one article:\n%s", rec.Body.String())
	}

	// The comics are read; everything else is untouched.
	for title, wantRead := range map[string]bool{
		"Comic Strip unread":  true,
		"Kernel Notes unread": false,
		"Loose Wire unread":   false,
	} {
		if got := isRead(t, tr, ids[title]); got != wantRead {
			t.Errorf("%q read = %v, want %v", title, got, wantRead)
		}
	}
}

// An article opened from the narrowed list has to remember *that* list, or the way
// back and the next article walk a list the reader was never in.
func TestAnArticleOpenedFromTheNarrowedListRemembersIt(t *testing.T) {
	rd, _, ids := filedFixture(t)

	body := rd.body("/?category=Comics")
	id := strconv.FormatInt(int64(ids["Comic Strip unread"]), 10)
	if !strings.Contains(body, "/articles/"+id+"?from=unread-category:Comics") &&
		!strings.Contains(body, "/articles/"+id+"?from=unread-category%3aComics") &&
		!strings.Contains(body, "/articles/"+id+"?from=unread-category%3AComics") {
		t.Errorf("the article link does not carry the narrowed list:\n%s", body)
	}

	article := rd.body("/articles/" + id + "?from=" + url.QueryEscape("unread-category:Comics"))
	if !strings.Contains(article, "Unread in Comics") {
		t.Errorf("the article page does not offer the way back to the narrowed list:\n%s", article)
	}
	if !strings.Contains(article, `href="/?category=Comics"`) {
		t.Errorf("the way back does not point at the narrowed list:\n%s", article)
	}
}

// A category whose name needs escaping has to survive the round trip through a token,
// an href and a form field. Folder names come from somebody else's reader.
func TestACategoryNameThatNeedsEscaping(t *testing.T) {
	rd, tr := readingFixture(t)
	const name = "News & Views: Q&A/misc"

	if _, _, err := tr.store.UpsertFeed(t.Context(), tr.alice, store.FeedParams{
		FeedURL: "https://awkward.example.com/feed.xml", Title: "Awkward", Category: name,
	}); err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	body := rd.body("/?category=" + url.QueryEscape(name))
	if !strings.Contains(body, escaped("Unread in "+name)) {
		t.Errorf("the page does not name the category correctly:\n%s", body)
	}

	// And the same list is reachable through its token, which is what the mark-read
	// control and every article link carry.
	confirm := rd.get("/mark-read?from=" + url.QueryEscape("unread-category:"+name))
	if confirm.Code != http.StatusOK {
		t.Fatalf("GET /mark-read for an escaped category = %d, want 200", confirm.Code)
	}
}

// isRead reports whether Alice has read an article.
//
// A subquery rather than a plain SELECT because an article nobody has touched has no
// `article_state` row at all — unread is the absence of a record, not a false in one.
func isRead(t *testing.T, tr twoReadersHTTP, id store.ArticleID) bool {
	t.Helper()

	var read bool
	if err := tr.pool.QueryRow(t.Context(),
		`SELECT COALESCE((SELECT read FROM article_state WHERE user_id = $1 AND article_id = $2), false)`,
		tr.alice, id).Scan(&read); err != nil {
		t.Fatalf("reading article state: %v", err)
	}
	return read
}
