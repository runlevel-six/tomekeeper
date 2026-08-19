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

// Removing a subscription. The interesting part is not the DELETE — it is what the
// confirmation promises beforehand and what survives afterwards, because an archive
// that quietly took articles away with a feed would be the one unrecoverable mistake
// this project can make.

func unsubscribePath(id store.FeedID) string {
	return "/feeds/" + strconv.FormatInt(int64(id), 10) + "/unsubscribe"
}

// carriedFeed subscribes Alice to a feed carrying one article, and returns both ids.
// touched decides whether she has read it, which is what keeps an article reachable
// after its feed is gone.
func carriedFeed(t *testing.T, tr twoReadersHTTP, name string, touched bool) (store.FeedID, store.ArticleID) {
	t.Helper()
	ctx := t.Context()

	feedID, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL: "https://" + name + ".example.com/feed.xml", Title: name,
	})
	if err != nil {
		t.Fatalf("UpsertFeed(%s) = %v", name, err)
	}

	published := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	articleID, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://" + name + ".example.com/post", Title: name + " post",
		PublishedAt: &published,
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if _, err := tr.store.InsertContent(ctx, store.ContentParams{
		ArticleID: articleID, ExtractorName: "trafilatura", ExtractorVersion: "2",
		ContentOrigin: store.OriginFetched, HTML: "<p>body</p>", Text: "body", WordCount: 20,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}
	if _, err := tr.store.InsertFeedItem(ctx, tr.alice, store.FeedItemParams{
		FeedID: feedID, ArticleID: articleID, GUID: "guid-" + name,
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}
	if touched {
		if _, err := tr.store.SetRead(ctx, tr.alice, articleID, true); err != nil {
			t.Fatalf("SetRead() = %v", err)
		}
	}

	return feedID, articleID
}

// The reported case, end to end: a feed listed twice by an import, where the row being
// edited is the one that never worked. Correcting its address is refused — and now the
// refusal says which subscription holds it and what to do.
func TestEditingOntoATakenAddressNamesTheOtherFeedAndOffersTheWayOut(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	// The working subscription, as an OPML import would have left it.
	working, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL: "https://follow.example.com/tech/rss", Title: "Tech Letter", Category: "Tech",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if err := tr.store.RecordPollSuccess(ctx, tr.alice, working, "", "", time.Hour); err != nil {
		t.Fatalf("RecordPollSuccess() = %v", err)
	}

	// The spare, at the old address, which has never fetched anything.
	spare, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL: "https://follow.example.com/tech", Title: "Tech Letter", Category: "Tech",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	rec := rd.do(http.MethodPost, editPath(spare), url.Values{
		"url": {"https://follow.example.com/tech/rss"}, "title": {"Tech Letter"}, "enabled": {"true"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST %s = %d, want 409\n%s", editPath(spare), rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Which subscription holds the address, said by name.
	if !strings.Contains(body, "Tech Letter") {
		t.Errorf("the refusal does not name the other subscription:\n%s", body)
	}
	// That it is the working one, so this row is the spare.
	if !strings.Contains(body, "already being polled successfully") {
		t.Errorf("the refusal does not say which of the two works:\n%s", body)
	}
	// And the way out, on the form the reader is already looking at.
	if !strings.Contains(body, "unsubscribe="+strconv.FormatInt(int64(spare), 10)) {
		t.Errorf("the refused edit offers no way to remove the spare:\n%s", body)
	}

	// Taking that way out leaves the working subscription alone.
	rec = rd.do(http.MethodPost, unsubscribePath(spare), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200\n%s", unsubscribePath(spare), rec.Code, rec.Body.String())
	}
	if _, err := tr.store.GetFeed(ctx, tr.alice, spare); !store.IsNotFound(err) {
		t.Errorf("the spare subscription survived: %v", err)
	}
	if _, err := tr.store.GetFeed(ctx, tr.alice, working); err != nil {
		t.Errorf("the working subscription went with it: %v", err)
	}
}

// The question comes before the act, and it has to say what the act costs.
func TestUnsubscribeAsksFirstAndSaysWhatItCosts(t *testing.T) {
	rd, tr := readingFixture(t)
	feedID, _ := carriedFeed(t, tr, "quiet", false)

	body := rd.body("/feeds?unsubscribe=" + strconv.FormatInt(int64(feedID), 10))

	for _, want := range []string{
		"Unsubscribe from quiet?",
		"https://quiet.example.com/feed.xml",
		"It has carried 1 article",
		// The article came from this feed alone and has never been opened, so the
		// honest warning is that it leaves the reader's lists.
		"1 of them came from this feed alone",
		"cannot be undone",
		`action="` + unsubscribePath(feedID) + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the confirmation does not say %q:\n%s", want, body)
		}
	}

	// The subscription form stands down while a destructive question is on the page.
	if strings.Contains(body, `name="url"`) {
		t.Error("the add form is drawn underneath the unsubscribe question")
	}
	// And nothing happened yet.
	if _, err := tr.store.GetFeed(t.Context(), tr.alice, feedID); err != nil {
		t.Errorf("asking removed the feed: %v", err)
	}
}

// A feed whose articles are safe should say so rather than warning about nothing.
func TestUnsubscribeSaysWhenNothingIsLost(t *testing.T) {
	rd, tr := readingFixture(t)
	// Read, so article_state keeps it reachable — the second half of the visibility
	// predicate, and the reason this is not a warning.
	feedID, _ := carriedFeed(t, tr, "read", true)

	body := rd.body("/feeds?unsubscribe=" + strconv.FormatInt(int64(feedID), 10))
	if !strings.Contains(body, "All of them stay in your lists") {
		t.Errorf("the confirmation does not say the articles are safe:\n%s", body)
	}
	if strings.Contains(body, "came from this feed alone") {
		t.Error("the confirmation warns about articles that are not at risk")
	}
}

// Unsubscribing removes the subscription and keeps the archive. This is the assertion
// the whole design rests on: articles are the root entity, not children of a feed.
func TestUnsubscribeKeepsTheArticles(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()
	feedID, articleID := carriedFeed(t, tr, "kept", true)

	rec := rd.do(http.MethodPost, unsubscribePath(feedID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200\n%s", unsubscribePath(feedID), rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Unsubscribed from <strong>kept</strong>") {
		t.Errorf("the page does not confirm the removal:\n%s", body)
	}
	if !strings.Contains(body, "still archived") {
		t.Errorf("the page does not say the articles were kept:\n%s", body)
	}

	if _, err := tr.store.GetFeed(ctx, tr.alice, feedID); !store.IsNotFound(err) {
		t.Errorf("the subscription survived: %v", err)
	}

	// The article is still there, with its body.
	if _, err := tr.store.GetArticle(ctx, articleID); err != nil {
		t.Errorf("the article went with the feed: %v", err)
	}
	var bodies int64
	if err := tr.pool.QueryRow(ctx,
		`SELECT count(*) FROM article_content WHERE article_id = $1`, articleID).Scan(&bodies); err != nil {
		t.Fatalf("counting bodies: %v", err)
	}
	if bodies == 0 {
		t.Error("the article's stored body went with the feed")
	}

	// And it is still reachable, because she had read it.
	if all := rd.body("/all"); !strings.Contains(all, "kept post") {
		t.Errorf("an article she had read left her lists with the feed:\n%s", all)
	}

	// The feed's items are gone, which is the only cascade there is: they record which
	// feed carried what, and that feed no longer exists.
	var items int64
	if err := tr.pool.QueryRow(ctx,
		`SELECT count(*) FROM feed_items WHERE feed_id = $1`, feedID).Scan(&items); err != nil {
		t.Fatalf("counting feed items: %v", err)
	}
	if items != 0 {
		t.Errorf("%d feed_items outlived their feed", items)
	}
}

// One reader cannot remove another's subscription, and a stale link is not an error
// page.
func TestUnsubscribeIsScopedAndForgiving(t *testing.T) {
	rd, tr := readingFixture(t)

	rec := rd.do(http.MethodPost, unsubscribePath(tr.bobFeed), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST %s = %d, want 404", unsubscribePath(tr.bobFeed), rec.Code)
	}
	if _, err := tr.store.GetFeed(t.Context(), tr.bob, tr.bobFeed); err != nil {
		t.Errorf("Bob's feed was removed: %v", err)
	}

	if rec := rd.do(http.MethodPost, "/feeds/999999/unsubscribe", nil); rec.Code != http.StatusNotFound {
		t.Errorf("POST for a feed that does not exist = %d, want 404", rec.Code)
	}

	// A question about a feed that has already gone — a link followed twice — leaves
	// the page as it was rather than erroring.
	body := rd.body("/feeds?unsubscribe=999999")
	if !strings.Contains(body, "Add a feed") {
		t.Errorf("a stale unsubscribe link did not fall back to the page:\n%s", body)
	}
	if strings.Contains(body, "Unsubscribe from") {
		t.Error("a stale unsubscribe link asked about a feed that does not exist")
	}
}

// The reader's ordering and filter survive the removal, like they survive a save.
func TestUnsubscribeKeepsTheReadersView(t *testing.T) {
	rd, tr := readingFixture(t)
	feedID, _ := carriedFeed(t, tr, "viewed", false)

	rec := rd.do(http.MethodPost, unsubscribePath(feedID)+"?q=alice&sort=title&dir=desc", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, `value="alice"`) {
		t.Errorf("the page came back without the filter:\n%s", body)
	}
	if !strings.Contains(body, `aria-sort="descending"`) {
		t.Errorf("the page came back unsorted:\n%s", body)
	}
}

// The edit form is where the control lives, and it has to be reachable from there.
func TestTheEditFormOffersUnsubscribe(t *testing.T) {
	rd, tr := readingFixture(t)
	feedID, _ := carriedFeed(t, tr, "offered", false)
	id := strconv.FormatInt(int64(feedID), 10)

	body := rd.body("/feeds?edit=" + id)
	if !strings.Contains(body, "unsubscribe="+id) {
		t.Errorf("the edit form offers no way to unsubscribe:\n%s", body)
	}

	// Not on the blank add form, which has no subscription to remove.
	if plain := rd.body("/feeds"); strings.Contains(plain, "Unsubscribe") {
		t.Errorf("the add form offers to unsubscribe:\n%s", plain)
	}
}
