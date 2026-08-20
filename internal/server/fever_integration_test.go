package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/asseturl"
	"github.com/runlevel-six/tomekeeper/internal/auth"
	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/server"
	"github.com/runlevel-six/tomekeeper/internal/session"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The Fever API, exercised the way a mobile client uses it.
//
// Every case here goes through the HTTP surface rather than calling the store,
// because the parts most likely to be wrong are the protocol's: which arguments
// arrive where, what a response member is called, and whether the credential is read
// at all. The store's own scoping has its own tests; what these add is that the API
// cannot reach around it.

// feverClient is a mobile client: a handler and an api_key, and deliberately no
// cookie jar. Nothing here may depend on a session.
type feverClient struct {
	t      *testing.T
	h      http.Handler
	apiKey string
}

// call makes one request the way a client does: read arguments in the query string,
// the credential and any write arguments in the POST body.
func (c *feverClient) call(query string, form url.Values) map[string]any {
	c.t.Helper()

	rec := c.post(query, form)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("POST /fever/?%s = %d, want 200\n%s", query, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		c.t.Fatalf("the response is not JSON: %v\n%s", err, rec.Body.String())
	}
	return payload
}

func (c *feverClient) post(query string, form url.Values) *httptest.ResponseRecorder {
	c.t.Helper()

	if form == nil {
		form = url.Values{}
	}
	if c.apiKey != "" {
		form.Set("api_key", c.apiKey)
	}

	path := "/fever/"
	if query != "" {
		path += "?api&" + query
	} else {
		path += "?api"
	}

	req := httptest.NewRequestWithContext(c.t.Context(), http.MethodPost, path,
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec
}

// authenticated asserts the response is an accepted one, and returns it.
func (c *feverClient) authenticated(query string, form url.Values) map[string]any {
	c.t.Helper()

	payload := c.call(query, form)
	if got := payload["auth"]; got != float64(1) {
		c.t.Fatalf("the request was not authenticated: auth = %v\n%v", got, payload)
	}
	if got := payload["api_version"]; got != float64(3) {
		c.t.Errorf("api_version = %v, want 3", got)
	}
	return payload
}

// ids reads one of the comma-separated id members.
func (c *feverClient) ids(payload map[string]any, member string) []string {
	c.t.Helper()

	raw, ok := payload[member]
	if !ok {
		c.t.Fatalf("the response has no %s member: %v", member, payload)
	}
	s, ok := raw.(string)
	if !ok {
		c.t.Fatalf("%s is %T, want a string of comma-separated ids", member, raw)
	}
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// items reads the items array as a list of maps.
func (c *feverClient) items(payload map[string]any) []map[string]any {
	c.t.Helper()

	raw, ok := payload["items"]
	if !ok {
		c.t.Fatalf("the response has no items member: %v", payload)
	}
	list, ok := raw.([]any)
	if !ok {
		c.t.Fatalf("items is %T, want an array", raw)
	}

	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			c.t.Fatalf("an item is %T, want an object", entry)
		}
		out = append(out, m)
	}
	return out
}

// feverFixture builds a client over the same two-reader data set the web tests use,
// so every case can also check that the other reader stays invisible.
func feverFixture(t *testing.T) (*feverClient, twoReadersHTTP) {
	t.Helper()

	tr := setupTwoReadersFor(t)
	seedPassword(t, tr)

	h := feverHandlerFor(t, tr, nil)

	return &feverClient{
		t: t, h: h,
		// The same derivation the seeded password used, which is the protocol's:
		// MD5 of username:password.
		apiKey: auth.FeverAPIKey("tome", testPassword),
	}, tr
}

// feverHandlerFor builds a server for the fixture, optionally with a blob store so
// that image URLs can be followed.
func feverHandlerFor(t *testing.T, tr twoReadersHTTP, blobs blob.Store) http.Handler {
	t.Helper()

	sessions, err := session.NewCookie([]byte(feverTestSecret), session.DefaultTTL, true)
	if err != nil {
		t.Fatalf("NewCookie() = %v", err)
	}
	signer, err := asseturl.NewSigner([]byte(feverTestSecret), asseturl.DefaultTTL)
	if err != nil {
		t.Fatalf("NewSigner() = %v", err)
	}

	return server.New(testConfig(), discardLogger(), server.Deps{
		Store: tr.store, Sessions: sessions, AssetURLs: signer, Blobs: blobs,
	}).Handler()
}

// The same secret for both, which is what production does: one configured value,
// two keys derived from it with different labels.
const feverTestSecret = "fever test secret"

// A client with no key, or the wrong one, is told so inside the body with HTTP 200 —
// which looks wrong and is the protocol. Clients read auth, not the status code.
func TestFeverRefusesAKeyItDoesNotKnow(t *testing.T) {
	c, _ := feverFixture(t)

	for name, key := range map[string]string{
		"no key":    "",
		"wrong key": "00000000000000000000000000000000",
		"not a key": "hello",
	} {
		t.Run(name, func(t *testing.T) {
			bad := &feverClient{t: t, h: c.h, apiKey: key}

			rec := bad.post("groups&feeds&items&unread_item_ids", nil)
			if rec.Code != http.StatusOK {
				t.Errorf("an unauthenticated fever request = %d, want 200 with auth:0", rec.Code)
			}

			var payload map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("the response is not JSON: %v\n%s", err, rec.Body.String())
			}
			if got := payload["auth"]; got != float64(0) {
				t.Errorf("auth = %v, want 0", got)
			}
			// And nothing else. A refusal that still answered the query would be a way
			// to read the archive without a credential.
			for _, member := range []string{"groups", "feeds", "items", "unread_item_ids",
				"last_refreshed_on_time"} {
				if _, present := payload[member]; present {
					t.Errorf("an unauthenticated response carried %q: %v", member, payload)
				}
			}
		})
	}
}

func TestFeverServesTheGroupsAndFeeds(t *testing.T) {
	c, tr := feverFixture(t)

	// Give Alice's feed a folder, so groups has something to say.
	if _, err := tr.store.UpdateFeed(t.Context(), tr.alice, tr.aliceFeed,
		store.FeedEdit{FeedURL: "https://alice.example.com/f.xml", Title: "Alice's Feed",
			Category: "Comics"}); err != nil {
		t.Fatalf("UpdateFeed() = %v", err)
	}

	payload := c.authenticated("groups&feeds", nil)

	feeds, ok := payload["feeds"].([]any)
	if !ok {
		t.Fatalf("feeds is %T, want an array: %v", payload["feeds"], payload)
	}
	if len(feeds) != 1 {
		t.Fatalf("got %d feeds, want only Alice's: %v", len(feeds), feeds)
	}

	feed := feeds[0].(map[string]any)
	if got := feed["title"]; got != "Alice's Feed" {
		t.Errorf("the feed title is %v", got)
	}
	if got := feed["url"]; got != "https://alice.example.com/f.xml" {
		t.Errorf("the feed url is %v", got)
	}
	// Sparks were Fever's low-priority feeds. Nothing here is one, and a client that
	// saw is_spark:1 would file the whole archive under a heading it invented.
	if got := feed["is_spark"]; got != float64(0) {
		t.Errorf("is_spark = %v, want 0", got)
	}
	if _, present := feed["favicon_id"]; !present {
		t.Error("the feed has no favicon_id, which clients read unconditionally")
	}

	groups, ok := payload["groups"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("got %v groups, want one for Comics", payload["groups"])
	}
	group := groups[0].(map[string]any)
	if got := group["title"]; got != "Comics" {
		t.Errorf("the group title is %v", got)
	}
	groupID, ok := group["id"].(float64)
	if !ok || groupID <= 0 {
		t.Errorf("the group id is %v, want a positive integer", group["id"])
	}

	// Both requests carry the memberships, and the membership has to point at the
	// group by the same id the group announced.
	byGroup, ok := payload["feeds_groups"].([]any)
	if !ok || len(byGroup) != 1 {
		t.Fatalf("got %v feeds_groups, want one: %v", payload["feeds_groups"], payload)
	}
	membership := byGroup[0].(map[string]any)
	if membership["group_id"] != groupID {
		t.Errorf("the membership names group %v but the group is %v",
			membership["group_id"], groupID)
	}
	if got := membership["feed_ids"]; got != strconv.FormatInt(int64(tr.aliceFeed), 10) {
		t.Errorf("feed_ids = %v, want Alice's feed id", got)
	}
}

// The point of the whole milestone: a client gets the extracted body, not the
// truncated summary the feed carried.
func TestFeverItemsCarryTheExtractedBody(t *testing.T) {
	c, tr := feverFixture(t)

	payload := c.authenticated("items", nil)

	items := c.items(payload)
	if len(items) != 1 {
		t.Fatalf("got %d items, want only Alice's article: %v", len(items), items)
	}

	item := items[0]
	if got := item["id"]; got != float64(tr.aliceOnly) {
		t.Errorf("the item id is %v, want the article id %d", got, tr.aliceOnly)
	}
	if got := item["feed_id"]; got != float64(tr.aliceFeed) {
		t.Errorf("feed_id = %v, want %d", got, tr.aliceFeed)
	}
	html, _ := item["html"].(string)
	if !strings.Contains(html, "distinctive alpaca passage") {
		t.Errorf("the item does not carry the stored body: %q", html)
	}
	if got := item["is_read"]; got != float64(0) {
		t.Errorf("is_read = %v, want 0", got)
	}
	if got := item["is_saved"]; got != float64(0) {
		t.Errorf("is_saved = %v, want 0", got)
	}
	// created_on_time is a Unix timestamp, and a client sorts on it.
	created, ok := item["created_on_time"].(float64)
	if !ok || created <= 0 {
		t.Errorf("created_on_time = %v, want a Unix timestamp", item["created_on_time"])
	}

	// total_items counts what the item pages can return, so a client knows whether to
	// keep asking.
	if got := payload["total_items"]; got != float64(1) {
		t.Errorf("total_items = %v, want 1", got)
	}
}

// An article with no stored body still says something a client can act on, rather
// than presenting a blank pane with no way onward. Webcomics are the real population
// here: image-only strips that no extractor can read.
func TestFeverSaysWhenThereIsNoStoredBody(t *testing.T) {
	c, tr := feverFixture(t)
	ctx := t.Context()

	id, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://comics.example.com/strip-1", Title: "A Strip",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if _, err := tr.store.InsertFeedItem(ctx, tr.alice, store.FeedItemParams{
		FeedID: tr.aliceFeed, ArticleID: id, GUID: "guid-strip-1",
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}

	for _, item := range c.items(c.authenticated("items", nil)) {
		if item["id"] != float64(id) {
			continue
		}
		html, _ := item["html"].(string)
		if !strings.Contains(html, "no stored copy") {
			t.Errorf("a bodyless item does not say so: %q", html)
		}
		if !strings.Contains(html, "https://comics.example.com/strip-1") {
			t.Errorf("a bodyless item does not link the original: %q", html)
		}
		return
	}
	t.Error("the bodyless article is missing from the items response entirely")
}

// The three paging arguments, including the one the specification words in a way that
// cannot be taken literally.
func TestFeverItemsPageTheWayClientsAsk(t *testing.T) {
	c, tr := feverFixture(t)
	ctx := t.Context()

	// Six more articles, so there is an order to page through.
	ids := []store.ArticleID{tr.aliceOnly}
	for i := range 6 {
		id, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
			URLCanonical: "https://example.com/paged-" + strconv.Itoa(i),
			Title:        "Paged " + strconv.Itoa(i),
		})
		if err != nil {
			t.Fatalf("UpsertArticle() = %v", err)
		}
		if _, err := tr.store.InsertFeedItem(ctx, tr.alice, store.FeedItemParams{
			FeedID: tr.aliceFeed, ArticleID: id, GUID: "guid-paged-" + strconv.Itoa(i),
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}
		ids = append(ids, id)
	}

	itemIDs := func(payload map[string]any) []store.ArticleID {
		t.Helper()
		var out []store.ArticleID
		for _, item := range c.items(payload) {
			out = append(out, store.ArticleID(item["id"].(float64)))
		}
		return out
	}

	t.Run("since_id walks forward, oldest first", func(t *testing.T) {
		got := itemIDs(c.authenticated("items&since_id="+idStr(ids[2]), nil))
		want := ids[3:]
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v (ascending, after the given id)", got, want)
			}
		}
	})

	t.Run("max_id walks backward, newest first", func(t *testing.T) {
		got := itemIDs(c.authenticated("items&max_id="+idStr(ids[3]), nil))
		if len(got) != 3 {
			t.Fatalf("got %v, want the three items below id %d", got, ids[3])
		}
		if got[0] != ids[2] || got[2] != ids[0] {
			t.Errorf("got %v, want descending from %d", got, ids[2])
		}
	})

	// The compatibility detail: taken literally, the specification's own
	// initial-sync instruction asks for items with an id below zero.
	t.Run("max_id=0 means the newest page", func(t *testing.T) {
		got := itemIDs(c.authenticated("items&max_id=0", nil))
		if len(got) != len(ids) {
			t.Fatalf("got %d items, want all %d", len(got), len(ids))
		}
		if got[0] != ids[len(ids)-1] {
			t.Errorf("got %v, want the newest first", got)
		}
	})

	t.Run("with_ids fetches exactly those", func(t *testing.T) {
		want := []store.ArticleID{ids[1], ids[4]}
		got := itemIDs(c.authenticated("items&with_ids="+idStr(want[0])+","+idStr(want[1]), nil))
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// The isolation test the scoping discipline requires, at the API rather than at the
// store: Alice's credential must not reach Bob's archive by any of the routes this
// protocol offers.
func TestFeverCannotReachAnotherReadersArchive(t *testing.T) {
	c, tr := feverFixture(t)
	ctx := t.Context()

	// Bob's article is starred and unread, so it would appear in every list below if
	// any of them were unscoped.
	if _, err := tr.store.SetStarred(ctx, tr.bob, tr.bobOnly, true); err != nil {
		t.Fatalf("SetStarred() = %v", err)
	}

	bobID := idStr(tr.bobOnly)

	t.Run("items", func(t *testing.T) {
		for _, item := range c.items(c.authenticated("items&max_id=0", nil)) {
			if item["id"] == float64(tr.bobOnly) {
				t.Error("the items response carries the other reader's article")
			}
		}
	})

	// Naming it directly is the sharper case: with_ids skips the ordering entirely,
	// so this asks the archive for one article by id.
	t.Run("with_ids naming it", func(t *testing.T) {
		if items := c.items(c.authenticated("items&with_ids="+bobID, nil)); len(items) != 0 {
			t.Errorf("with_ids returned another reader's article: %v", items)
		}
	})

	t.Run("unread_item_ids", func(t *testing.T) {
		for _, id := range c.ids(c.authenticated("unread_item_ids", nil), "unread_item_ids") {
			if id == bobID {
				t.Error("unread_item_ids names the other reader's article")
			}
		}
	})

	t.Run("saved_item_ids", func(t *testing.T) {
		for _, id := range c.ids(c.authenticated("saved_item_ids", nil), "saved_item_ids") {
			if id == bobID {
				t.Error("saved_item_ids names the other reader's article")
			}
		}
	})

	t.Run("feeds", func(t *testing.T) {
		feeds, _ := c.authenticated("feeds", nil)["feeds"].([]any)
		for _, raw := range feeds {
			if raw.(map[string]any)["id"] == float64(tr.bobFeed) {
				t.Error("the feeds response carries the other reader's subscription")
			}
		}
	})
}

func TestFeverMarksAnItemReadAndBack(t *testing.T) {
	c, tr := feverFixture(t)
	id := idStr(tr.aliceOnly)

	// A mark response carries the updated id lists whether or not they were asked
	// for, which is what keeps a client's cache correct after a write it did not
	// follow with a read.
	payload := c.authenticated("", url.Values{
		"mark": {"item"}, "as": {"read"}, "id": {id},
	})
	if unread := c.ids(payload, "unread_item_ids"); len(unread) != 0 {
		t.Errorf("the article is still unread after being marked read: %v", unread)
	}
	if _, present := payload["saved_item_ids"]; !present {
		t.Error("a mark response does not carry saved_item_ids")
	}

	view, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if !view.Read {
		t.Error("the article was not actually marked read")
	}

	// as=unread is not in the specification's list for mark=item, and is the only way
	// a reader can undo a mistaken tap. Clients send it.
	payload = c.authenticated("", url.Values{
		"mark": {"item"}, "as": {"unread"}, "id": {id},
	})
	if unread := c.ids(payload, "unread_item_ids"); len(unread) != 1 || unread[0] != id {
		t.Errorf("unread_item_ids = %v after as=unread, want just %s", unread, id)
	}
}

// Fever's "saved" is this archive's starred, which is the only one of the two that
// round-trips — see StarredArticleIDs for why saved_at cannot be it.
func TestFeverSavingAnItemStarsIt(t *testing.T) {
	c, tr := feverFixture(t)
	id := idStr(tr.aliceOnly)

	payload := c.authenticated("", url.Values{
		"mark": {"item"}, "as": {"saved"}, "id": {id},
	})
	if saved := c.ids(payload, "saved_item_ids"); len(saved) != 1 || saved[0] != id {
		t.Errorf("saved_item_ids = %v, want just %s", saved, id)
	}

	view, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if !view.Starred {
		t.Error("as=saved did not star the article")
	}

	payload = c.authenticated("", url.Values{
		"mark": {"item"}, "as": {"unsaved"}, "id": {id},
	})
	if saved := c.ids(payload, "saved_item_ids"); len(saved) != 0 {
		t.Errorf("saved_item_ids = %v after as=unsaved, want none", saved)
	}
}

// Marking by id is bounded by visibility, in the store rather than here. This is the
// test that says so through the API.
func TestFeverCannotMarkAnotherReadersArticle(t *testing.T) {
	c, tr := feverFixture(t)

	c.authenticated("", url.Values{
		"mark": {"item"}, "as": {"read"}, "id": {idStr(tr.bobOnly)},
	})

	// Read as Bob, because as Alice the article is not visible at all and a "not
	// found" would prove nothing about whether a row was written.
	view, err := tr.store.ArticleForUser(t.Context(), tr.bob, tr.bobOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if view.Read {
		t.Error("one reader's api key marked another reader's article read")
	}
}

// `before` is the guard that stops a bulk mark reaching items the client has never
// shown anybody. It is the reason mark=feed and mark=group take a timestamp at all.
func TestFeverMarkFeedHonorsBefore(t *testing.T) {
	c, tr := feverFixture(t)
	ctx := t.Context()

	// Two articles either side of the moment the client last synced. Both dates are
	// explicit rather than "whenever the fixture ran", because `before` is a
	// whole-second Unix timestamp: an article ingested in the same second as the
	// timestamp falls on the boundary, and a test that straddled it would pass or fail
	// on sub-second luck. The reference documents that boundary; it is conservative,
	// excluding rather than over-marking.
	add := func(slug string, published time.Time) store.ArticleID {
		t.Helper()
		id, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
			URLCanonical: "https://example.com/" + slug, Title: slug,
			PublishedAt: ptr(published),
		})
		if err != nil {
			t.Fatalf("UpsertArticle() = %v", err)
		}
		if _, err := tr.store.InsertFeedItem(ctx, tr.alice, store.FeedItemParams{
			FeedID: tr.aliceFeed, ArticleID: id, GUID: "guid-" + slug,
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}
		return id
	}

	seen := add("already-synced", time.Now().Add(-2*time.Hour))
	unseen := add("arrived-later", time.Now().Add(2*time.Hour))

	c.authenticated("", url.Values{
		"mark":   {"feed"},
		"as":     {"read"},
		"id":     {strconv.FormatInt(int64(tr.aliceFeed), 10)},
		"before": {strconv.FormatInt(time.Now().Unix(), 10)},
	})

	read := func(id store.ArticleID) bool {
		t.Helper()
		view, err := tr.store.ArticleForUser(ctx, tr.alice, id)
		if err != nil {
			t.Fatalf("ArticleForUser() = %v", err)
		}
		return view.Read
	}

	if !read(seen) {
		t.Error("the article the client had already seen was not marked read")
	}
	if read(unseen) {
		t.Error("an article newer than `before` was marked read, which is what `before` exists to prevent")
	}
}

// A feed id belonging to somebody else selects nothing, because the filter carries
// its own user scoping rather than trusting the id.
//
// The article has to be one *both* readers can see, and that is the whole design of
// this test. The obvious version — Alice marks Bob's feed, assert Bob's private
// article is untouched — passes with the feed filter's `f2.user_id` deleted, because
// the shared visibility predicate excludes that article anyway. It tests the wrong
// clause and reports success. A syndicated story carried by both readers' feeds is
// visible to Alice, so visibility lets it through and the only thing standing between
// her api_key and it is the feed filter's own scoping. Verified by neutering that
// clause on its own.
func TestFeverCannotMarkAnotherReadersFeed(t *testing.T) {
	c, tr := feverFixture(t)
	ctx := t.Context()

	shared, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://example.com/syndicated", Title: "Carried By Both",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	for _, ref := range []struct {
		user store.UserID
		feed store.FeedID
	}{{tr.alice, tr.aliceFeed}, {tr.bob, tr.bobFeed}} {
		if _, err := tr.store.InsertFeedItem(ctx, ref.user, store.FeedItemParams{
			FeedID: ref.feed, ArticleID: shared, GUID: "guid-syndicated",
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}
	}

	c.authenticated("", url.Values{
		"mark": {"feed"}, "as": {"read"},
		"id": {strconv.FormatInt(int64(tr.bobFeed), 10)},
	})

	// Read as Alice: the question is whether her own credential, naming a feed that is
	// not hers, reached an article she can see.
	view, err := tr.store.ArticleForUser(ctx, tr.alice, shared)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if view.Read {
		t.Error("marking another reader's feed read marked a story that reader's feed carried")
	}

	// And the private article, which the visibility predicate covers. Kept because it
	// is the assertion somebody will expect to find here, and it is cheap.
	private, err := tr.store.ArticleForUser(ctx, tr.bob, tr.bobOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if private.Read {
		t.Error("one reader's api key marked another reader's private article read")
	}
}

// Group zero is Fever's "Kindling" super group: everything. It is the one place in
// this application where a bulk mark is meant to reach the whole archive, and it
// still may not reach past the reader's own.
func TestFeverMarkGroupZeroMarksEverythingVisible(t *testing.T) {
	c, tr := feverFixture(t)

	c.authenticated("", url.Values{"mark": {"group"}, "as": {"read"}, "id": {"0"}})

	mine, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if !mine.Read {
		t.Error("marking the whole archive read left the reader's own article unread")
	}

	theirs, err := tr.store.ArticleForUser(t.Context(), tr.bob, tr.bobOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if theirs.Read {
		t.Error("marking the whole archive read reached another reader's article")
	}
}

// A negative group id is the "Sparks" super group. Nothing here is a spark, so this
// is a genuine no-op — and specifically must not fall through to meaning everything.
func TestFeverMarkingSparksDoesNothing(t *testing.T) {
	c, tr := feverFixture(t)

	c.authenticated("", url.Values{"mark": {"group"}, "as": {"read"}, "id": {"-1"}})

	view, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if view.Read {
		t.Error("marking the always-empty sparks group read marked the archive read")
	}
}

// A real group id has to resolve back to the category it was minted from, and mark
// only that folder.
func TestFeverMarkGroupResolvesTheCategory(t *testing.T) {
	c, tr := feverFixture(t)
	ctx := t.Context()

	if _, err := tr.store.UpdateFeed(ctx, tr.alice, tr.aliceFeed,
		store.FeedEdit{FeedURL: "https://alice.example.com/f.xml", Title: "Alice's Feed",
			Category: "Comics"}); err != nil {
		t.Fatalf("UpdateFeed() = %v", err)
	}

	// A second subscription in a different folder, whose article must be left alone.
	otherFeed, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL: "https://alice.example.com/tech.xml", Title: "Tech Feed", Category: "Tech",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	techArticle, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://example.com/tech-1", Title: "A Tech Article",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if _, err := tr.store.InsertFeedItem(ctx, tr.alice, store.FeedItemParams{
		FeedID: otherFeed, ArticleID: techArticle, GUID: "guid-tech-1",
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}

	// The id comes from the groups response, which is the only place a client gets
	// one — so this exercises the mint and the lookup as a pair.
	groups := c.authenticated("groups", nil)["groups"].([]any)
	var comicsID string
	for _, raw := range groups {
		g := raw.(map[string]any)
		if g["title"] == "Comics" {
			comicsID = strconv.FormatInt(int64(g["id"].(float64)), 10)
		}
	}
	if comicsID == "" {
		t.Fatalf("the groups response has no Comics group: %v", groups)
	}

	c.authenticated("", url.Values{"mark": {"group"}, "as": {"read"}, "id": {comicsID}})

	comics, err := tr.store.ArticleForUser(ctx, tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if !comics.Read {
		t.Error("marking the Comics group read left its article unread")
	}

	tech, err := tr.store.ArticleForUser(ctx, tr.alice, techArticle)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if tech.Read {
		t.Error("marking one group read reached an article in another group")
	}
}

// The deliberate deviation: a client may POST a mark to a URL that also names read
// arguments, and both have to happen. Dispatching on the first match — as Miniflux
// does — would drop the write silently.
func TestFeverAnswersAReadAndAWriteInOneRequest(t *testing.T) {
	c, tr := feverFixture(t)

	payload := c.authenticated("items", url.Values{
		"mark": {"item"}, "as": {"read"}, "id": {idStr(tr.aliceOnly)},
	})

	if _, present := payload["items"]; !present {
		t.Error("the read half of a combined request was dropped")
	}

	view, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}
	if !view.Read {
		t.Error("the write half of a combined request was dropped")
	}
}

// Members a client may ask for that this archive has nothing to put in. An empty
// array rather than an absent member, so a client that reads the field unconditionally
// keeps working.
func TestFeverAnswersTheMembersItDoesNotImplement(t *testing.T) {
	c, _ := feverFixture(t)

	payload := c.authenticated("favicons&links", nil)

	for _, member := range []string{"favicons", "links"} {
		list, ok := payload[member].([]any)
		if !ok {
			t.Errorf("%s is %T, want an empty array", member, payload[member])
			continue
		}
		if len(list) != 0 {
			t.Errorf("%s = %v, want empty", member, list)
		}
	}
}

// The whole reason the signer exists: a client renders the body in its own view with
// no cookie, so the images in it have to be absolute and fetchable on their own.
func TestFeverImagesAreAbsoluteAndFetchableWithoutASession(t *testing.T) {
	tr := setupTwoReadersFor(t)
	seedPassword(t, tr)
	ctx := t.Context()

	// A blob store with one image in it, at the path the body will reference.
	root := t.TempDir()
	const assetPath = "assets/sha256/a1/b2/a1b2c3.avif"
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(assetPath)), 0o750); err != nil {
		t.Fatalf("creating the asset directory: %v", err)
	}
	const imageBytes = "not really an avif, but distinctive"
	if err := os.WriteFile(filepath.Join(root, assetPath), []byte(imageBytes), 0o640); err != nil {
		t.Fatalf("writing the asset: %v", err)
	}
	blobs, err := blob.NewFilesystem(root)
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	// A body that references it the way a stored body does: root-relative.
	if _, err := tr.store.InsertContent(ctx, store.ContentParams{
		ArticleID: tr.aliceOnly, ExtractorName: "trafilatura", ExtractorVersion: "4",
		ContentOrigin: store.OriginFetched,
		HTML:          `<p>Look</p><img src="/` + assetPath + `" alt="a picture">`,
		Text:          "Look", WordCount: 1,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}

	h := feverHandlerFor(t, tr, blobs)
	c := &feverClient{t: t, h: h, apiKey: auth.FeverAPIKey("tome", testPassword)}

	items := c.items(c.authenticated("items", nil))
	if len(items) == 0 {
		t.Fatal("no items came back")
	}
	html, _ := items[0]["html"].(string)

	src := imageSrc(t, html)
	if !strings.HasPrefix(src, "https://") {
		t.Errorf("the image source is not absolute: %q", src)
	}
	if !strings.Contains(src, asseturl.SignatureParam+"=") {
		t.Errorf("the image source carries no signature: %q", src)
	}

	// Follow it with no cookie at all, which is the client's situation.
	parsed, err := url.Parse(src)
	if err != nil {
		t.Fatalf("the image source is not a URL: %v", err)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		parsed.Path+"?"+parsed.RawQuery, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("fetching a signed image with no session = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != imageBytes {
		t.Errorf("the image bytes are %q", got)
	}

	// And the boundary that makes this safe: without the signature it is still closed.
	bare := httptest.NewRequestWithContext(ctx, http.MethodGet, parsed.Path, nil)
	bareRec := httptest.NewRecorder()
	h.ServeHTTP(bareRec, bare)
	if bareRec.Code == http.StatusOK {
		t.Error("an unsigned image request with no session was served")
	}
}

// imageSrc pulls the first img src out of a body.
func imageSrc(t *testing.T, html string) string {
	t.Helper()

	_, rest, found := strings.Cut(html, `<img src="`)
	if !found {
		t.Fatalf("the body has no image: %q", html)
	}
	src, _, found := strings.Cut(rest, `"`)
	if !found {
		t.Fatalf("the image source is unterminated: %q", html)
	}
	return src
}

func idStr(id store.ArticleID) string { return strconv.FormatInt(int64(id), 10) }

func ptr[T any](v T) *T { return &v }
