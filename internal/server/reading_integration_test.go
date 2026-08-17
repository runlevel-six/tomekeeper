package server_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/runlevel-six/tomekeeper/internal/auth"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/server"
	"github.com/runlevel-six/tomekeeper/internal/session"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// reader is a signed-in browser: a handler plus a cookie jar.
type reader struct {
	t    *testing.T
	h    http.Handler
	jar  []*http.Cookie
	user store.UserID
}

func (rd *reader) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	rd.t.Helper()

	var req *http.Request
	if form == nil {
		req = httptest.NewRequestWithContext(rd.t.Context(), method, path, nil)
	} else {
		req = httptest.NewRequestWithContext(rd.t.Context(), method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range rd.jar {
		req.AddCookie(c)
	}

	rec := httptest.NewRecorder()
	rd.h.ServeHTTP(rec, req)
	return rec
}

func (rd *reader) get(path string) *httptest.ResponseRecorder {
	return rd.do(http.MethodGet, path, nil)
}

// body fetches a page and fails the test unless it rendered.
func (rd *reader) body(path string) string {
	rd.t.Helper()

	rec := rd.get(path)
	if rec.Code != http.StatusOK {
		rd.t.Fatalf("GET %s = %d, want 200\n%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// readingFixture builds a signed-in reader over the two-user data set, so every
// test here can also check that the other reader's articles stay invisible.
func readingFixture(t *testing.T) (*reader, twoReadersHTTP) {
	t.Helper()

	tr := setupTwoReadersFor(t)

	sessions, err := session.NewCookie([]byte("reading test secret"), session.DefaultTTL, true)
	if err != nil {
		t.Fatalf("NewCookie() = %v", err)
	}

	p := auth.DefaultParams()
	p.Memory, p.Iterations = 8*1024, 1
	hash, err := auth.HashWith(p, testPassword)
	if err != nil {
		t.Fatalf("HashWith() = %v", err)
	}
	if err := tr.store.System().SetPassword(t.Context(), tr.alice, hash,
		auth.FeverAPIKey("tome", testPassword)); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}

	srv := server.New(testConfig(), discardLogger(),
		server.Deps{Store: tr.store, Sessions: sessions})

	rd := &reader{t: t, h: srv.Handler(), user: tr.alice}

	login := postLogin(t, rd.h, "tome", testPassword)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("signing in the fixture = %d", login.Code)
	}
	rd.jar = login.Result().Cookies()

	return rd, tr
}

// twoReadersHTTP is the same shape as the store package's fixture. Duplicated
// rather than exported, because a test helper crossing a package boundary is a
// dependency between test suites that nothing else needs.
type twoReadersHTTP struct {
	store              *store.Store
	pool               *pgxpool.Pool
	alice, bob         store.UserID
	aliceFeed, bobFeed store.FeedID
	aliceOnly, bobOnly store.ArticleID
}

func setupTwoReadersFor(t *testing.T) twoReadersHTTP {
	t.Helper()

	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	var bob store.UserID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("creating the second user: %v", err)
	}

	tr := twoReadersHTTP{store: s, pool: pool, alice: alice, bob: bob}

	mk := func(userID store.UserID, feedTitle, feedURL, slug, title, body string) (store.FeedID, store.ArticleID) {
		t.Helper()
		feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{FeedURL: feedURL, Title: feedTitle})
		if err != nil {
			t.Fatalf("UpsertFeed() = %v", err)
		}
		id, _, err := s.UpsertArticle(ctx, store.ArticleParams{
			URLCanonical: "https://example.com/" + slug, Title: title, SiteName: "Example",
		})
		if err != nil {
			t.Fatalf("UpsertArticle() = %v", err)
		}
		if _, err := s.InsertContent(ctx, store.ContentParams{
			ArticleID: id, ExtractorName: "trafilatura", ExtractorVersion: "2",
			ContentOrigin: store.OriginFetched,
			HTML:          "<p>" + body + "</p>", Text: body, WordCount: 20,
		}); err != nil {
			t.Fatalf("InsertContent() = %v", err)
		}
		if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
			FeedID: feedID, ArticleID: id, GUID: "guid-" + slug,
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}
		return feedID, id
	}

	tr.aliceFeed, tr.aliceOnly = mk(alice, "Alice's Feed", "https://alice.example.com/f.xml",
		"alice-only", "Alice's Article", "A distinctive alpaca passage that only Alice can read.")
	tr.bobFeed, tr.bobOnly = mk(bob, "Bob's Feed", "https://bob.example.com/f.xml",
		"bob-only", "Bob's Article", "A distinctive nautilus passage that only Bob can read.")

	return tr
}

func TestStreamPageRenders(t *testing.T) {
	rd, tr := readingFixture(t)

	body := rd.body("/")

	if !strings.Contains(body, "Alice&#39;s Article") && !strings.Contains(body, "Alice's Article") {
		t.Errorf("the stream does not list Alice's article:\n%s", body)
	}
	if strings.Contains(body, "Bob&#39;s Article") || strings.Contains(body, "Bob's Article") {
		t.Error("the stream lists Bob's article")
	}
	if !strings.Contains(body, `href="/articles/`+strconv.FormatInt(int64(tr.aliceOnly), 10)+`"`) {
		t.Error("the entry does not link to the reader")
	}
	// The keyboard hint is part of the interface brief's promise that this is usable from the
	// keyboard from day one.
	if !strings.Contains(body, "<kbd>j</kbd>") {
		t.Error("the stream does not mention the keyboard shortcuts")
	}
}

func TestEveryReadingViewRenders(t *testing.T) {
	rd, tr := readingFixture(t)

	for _, path := range []string{
		"/", "/all", "/starred", "/feeds", "/attention", "/search",
		"/search?q=alpaca",
		"/feeds/" + strconv.FormatInt(int64(tr.aliceFeed), 10),
		"/articles/" + strconv.FormatInt(int64(tr.aliceOnly), 10),
	} {
		t.Run(path, func(t *testing.T) {
			body := rd.body(path)
			if !strings.Contains(body, "</html>") {
				t.Errorf("the page is truncated:\n%s", body)
			}
			// Signed-in chrome on every page, so navigation is never a dead end.
			if !strings.Contains(body, `class="brand"`) {
				t.Error("the page is missing the site chrome")
			}
		})
	}
}

// The reader is where the archived body is shown, and where opening an article
// marks it read.
func TestArticleReaderShowsBodyAndMarksRead(t *testing.T) {
	rd, tr := readingFixture(t)
	path := "/articles/" + strconv.FormatInt(int64(tr.aliceOnly), 10)

	body := rd.body(path)
	if !strings.Contains(body, "distinctive alpaca passage") {
		t.Errorf("the stored body is not rendered:\n%s", body)
	}
	if !strings.Contains(body, `rel="noopener noreferrer"`) {
		t.Error("the link to the original is missing rel=noopener noreferrer")
	}

	view, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if !view.Read {
		t.Error("opening an article did not mark it read")
	}
}

// The access boundary, now through HTTP rather than the store: not found, so the
// response does not confirm the article exists.
func TestReadingViewsHideTheOtherReader(t *testing.T) {
	rd, tr := readingFixture(t)

	bobArticle := "/articles/" + strconv.FormatInt(int64(tr.bobOnly), 10)
	if rec := rd.get(bobArticle); rec.Code != http.StatusNotFound {
		t.Errorf("GET %s = %d, want 404\n%s", bobArticle, rec.Code, rec.Body.String())
	}

	bobFeed := "/feeds/" + strconv.FormatInt(int64(tr.bobFeed), 10)
	if rec := rd.get(bobFeed); rec.Code != http.StatusNotFound {
		t.Errorf("GET %s = %d, want 404", bobFeed, rec.Code)
	}

	// And search, which the scoping discipline names specifically.
	body := rd.body("/search?q=nautilus")
	if strings.Contains(body, "nautilus passage") {
		t.Errorf("search surfaced Bob's article to Alice:\n%s", body)
	}
	if strings.Contains(body, "Bob&#39;s Article") || strings.Contains(body, "Bob's Article") {
		t.Error("search surfaced Bob's article title to Alice")
	}

	// State changes are refused too, and as 404 rather than 403.
	rec := rd.do(http.MethodPost, bobArticle+"/star", url.Values{"on": {"true"}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST %s/star = %d, want 404", bobArticle, rec.Code)
	}
}

func TestSearchHighlightsAndFinds(t *testing.T) {
	rd, _ := readingFixture(t)

	body := rd.body("/search?q=alpaca")

	if !strings.Contains(body, "<mark>alpaca</mark>") {
		t.Errorf("the snippet does not highlight the match:\n%s", body)
	}
	if !strings.Contains(body, "1 result") {
		t.Errorf("the result count is missing:\n%s", body)
	}
}

// A snippet is article text, so anything a writer typed must arrive inert. This is
// the escaping in the `snippet` template function, which is the one place the
// archive's text is deliberately turned back into markup.
func TestSearchSnippetEscapesArticleMarkup(t *testing.T) {
	rd, tr := readingFixture(t)

	// An article that legitimately talks about a script tag.
	if _, err := tr.pool.Exec(t.Context(), `
		UPDATE article_content
		SET content_text = 'Writing <script>alert(1)</script> in a post about xsstopic markup'
		WHERE article_id = $1 AND is_current`, tr.aliceOnly); err != nil {
		t.Fatalf("seeding the body: %v", err)
	}

	body := rd.body("/search?q=xsstopic")

	// Scoped to the snippet element, not the whole page: the base template
	// legitimately contains a <script src> for the vendored htmx, so searching the
	// document for "<script" would match that and prove nothing.
	snippet := snippetElement(t, body)

	for _, dangerous := range []string{"<script", "</script", "<img", "onerror"} {
		if strings.Contains(snippet, dangerous) {
			t.Errorf("the article's markup survived into the snippet (%q): %q", dangerous, snippet)
		}
	}
	if !strings.Contains(snippet, "<mark>xsstopic</mark>") {
		t.Errorf("the highlight was lost: %q", snippet)
	}

	// Note what this test does *not* prove. ts_headline discards tag-like tokens
	// itself, so the escaping in the `snippet` template function is never reached
	// by this path. That is defense in depth rather than redundancy — Postgres's
	// tokenizer is not a sanitizer and is not documented as one — and the escaping
	// is covered directly by TestSnippetEscaping in the package's own tests.
}

// Toggling through the htmx endpoint returns just the control, with the new state.
func TestToggleReturnsTheRefreshedControl(t *testing.T) {
	rd, tr := readingFixture(t)
	path := "/articles/" + strconv.FormatInt(int64(tr.aliceOnly), 10) + "/star"

	rec := rd.do(http.MethodPost, path, url.Values{"on": {"true"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d\n%s", path, rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Error("the toggle returned a whole document rather than a fragment")
	}
	if !strings.Contains(body, `aria-pressed="true"`) {
		t.Errorf("the returned control does not show the new state:\n%s", body)
	}
	// The next press must offer to undo, not repeat.
	if !strings.Contains(body, `value="false"`) {
		t.Errorf("the returned control does not offer to unstar:\n%s", body)
	}

	view, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if !view.Starred {
		t.Error("the article was not starred")
	}
}

// The desired state comes from the form, so a repeated request is idempotent
// rather than a toggle that lands back where it started.
func TestToggleIsIdempotent(t *testing.T) {
	rd, tr := readingFixture(t)
	path := "/articles/" + strconv.FormatInt(int64(tr.aliceOnly), 10) + "/star"

	for range 3 {
		if rec := rd.do(http.MethodPost, path, url.Values{"on": {"true"}}); rec.Code != http.StatusOK {
			t.Fatalf("POST %s = %d", path, rec.Code)
		}
	}

	view, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if !view.Starred {
		t.Error("three identical star requests left the article unstarred")
	}
}

// The vendored asset the base template references has to exist, or every page
// silently loses its behavior.
func TestVendoredAndStaticAssetsAreServed(t *testing.T) {
	rd, _ := readingFixture(t)

	for _, path := range []string{
		"/static/tome.css",
		"/static/tome.js",
		"/static/vendor/htmx-2.0.9.min.js",
	} {
		rec := rd.get(path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 — the base template references it", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s returned an empty body", path)
		}
	}

	// And the page actually points at the file that exists, rather than a version
	// left behind by a half-finished upgrade.
	body := rd.body("/")
	if !strings.Contains(body, "/static/vendor/htmx-2.0.9.min.js") {
		t.Error("the base template does not reference the vendored htmx file")
	}
}

func TestPagesCarryAScriptPolicy(t *testing.T) {
	rd, _ := readingFixture(t)

	rec := rd.get("/")
	csp := rec.Header().Get("Content-Security-Policy")

	// script-src 'self' is what lets the vendored htmx run while still refusing
	// inline script and third-party hosts. Without it the pages load and quietly
	// do nothing.
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP has no script-src 'self', so the vendored script is blocked: %s", csp)
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP allows unsafe-inline: %s", csp)
	}
}

// Paging must not lose or repeat articles through the HTTP layer either, and an
// htmx request for a later page gets rows rather than a document.
func TestStreamPagingOverHTTP(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	// Enough articles to need three pages.
	for i := range store.DefaultStreamLimit*2 + 5 {
		slug := "page-" + strconv.Itoa(i)
		id, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
			URLCanonical: "https://example.com/" + slug, Title: "Paged " + strconv.Itoa(i),
		})
		if err != nil {
			t.Fatalf("UpsertArticle() = %v", err)
		}
		if _, err := tr.store.InsertContent(ctx, store.ContentParams{
			ArticleID: id, ExtractorName: "trafilatura", ExtractorVersion: "2",
			ContentOrigin: store.OriginFetched, HTML: "<p>b</p>", Text: "body", WordCount: 1,
		}); err != nil {
			t.Fatalf("InsertContent() = %v", err)
		}
		if _, err := tr.store.InsertFeedItem(ctx, tr.alice, store.FeedItemParams{
			FeedID: tr.aliceFeed, ArticleID: id, GUID: "g-" + slug,
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}
	}

	seen := map[string]int{}
	path := "/all"
	for page := 0; page < 6; page++ {
		body := rd.body(path)
		for _, id := range articleLinks(body) {
			seen[id]++
		}

		next := nextPageLink(body)
		if next == "" {
			break
		}
		path = next
	}

	// The seeded articles plus Alice's own fixture article. Bob's is deliberately
	// not counted: it is not visible to her, which is the whole point of the
	// isolation tests above.
	want := store.DefaultStreamLimit*2 + 5 + 1
	if len(seen) != want {
		t.Errorf("paging saw %d distinct articles, want %d", len(seen), want)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("article %s appeared %d times across pages", id, n)
		}
	}
}

// snippetElement returns the contents of the search result's snippet paragraph.
func snippetElement(t *testing.T, body string) string {
	t.Helper()

	m := snippetPara.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no snippet paragraph in the page:\n%s", body)
	}
	return m[1]
}

var (
	snippetPara = regexp.MustCompile(`(?s)<p class="snippet">(.*?)</p>`)
	articleHref = regexp.MustCompile(`href="/articles/(\d+)"`)
	nextHref    = regexp.MustCompile(`hx-get="([^"]+)"`)
)

func articleLinks(body string) []string {
	var out []string
	for _, m := range articleHref.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func nextPageLink(body string) string {
	m := nextHref.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[1], "&amp;", "&")
}
