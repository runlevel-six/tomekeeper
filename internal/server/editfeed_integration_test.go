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

// Editing a subscription, which until now was an UPDATE somebody had to write by
// hand. These cover the part that is not obvious from the form: what an edit does to
// polling, and what it refuses to do.

// editPath is the route for one feed's form.
func editPath(id store.FeedID) string {
	return "/feeds/" + strconv.FormatInt(int64(id), 10) + "/edit"
}

// brokenFeed is one of Alice's subscriptions with a poll history: validators from
// the old endpoint, a run of failures, and a next poll a long way off.
func brokenFeed(t *testing.T, tr twoReadersHTTP, feedURL string) store.FeedID {
	t.Helper()
	ctx := t.Context()

	id, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL: feedURL, Title: "Moved Publication", Category: "News",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if err := tr.store.RecordPollSuccess(ctx, tr.alice, id, `"etag-from-the-old-url"`,
		"Mon, 04 Aug 2026 09:00:00 GMT", 12*time.Hour); err != nil {
		t.Fatalf("RecordPollSuccess() = %v", err)
	}
	for range 3 {
		if _, err := tr.store.RecordPollFailure(ctx, tr.alice, id, "HTTP 404", 24*time.Hour, 20); err != nil {
			t.Fatalf("RecordPollFailure() = %v", err)
		}
	}
	return id
}

// nextPollWithin reports whether a feed is due within d, which is how "queued for a
// poll now" is visible from outside the worker.
func nextPollWithin(t *testing.T, tr twoReadersHTTP, id store.FeedID, d time.Duration) bool {
	t.Helper()

	var due bool
	if err := tr.pool.QueryRow(t.Context(),
		`SELECT next_poll_at <= now() + $2::interval FROM feeds WHERE id = $1`,
		id, d.String()).Scan(&due); err != nil {
		t.Fatalf("reading next_poll_at: %v", err)
	}
	return due
}

// The form opens with the feed in it, and posts to that feed rather than to the add
// route — the whole hazard of sharing one form is a Save that creates a second
// subscription instead of correcting the first.
func TestEditFormOpensWithTheFeedInIt(t *testing.T) {
	rd, tr := readingFixture(t)
	id := brokenFeed(t, tr, "https://news.example.com/old.xml")

	body := rd.body("/feeds?edit=" + strconv.FormatInt(int64(id), 10))

	for _, want := range []string{
		`action="` + editPath(id) + `"`,
		`value="https://news.example.com/old.xml"`,
		`value="Moved Publication"`,
		`value="News"`,
		`name="enabled" value="true" checked`,
		"Save changes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the edit form does not carry %q:\n%s", want, body)
		}
	}
}

// A stale link — a feed that has since gone, or somebody else's id — falls back to
// the blank add form rather than to an error page. The reader asked for the feeds
// page; they get the feeds page.
func TestEditFormIgnoresAnUnknownFeed(t *testing.T) {
	rd, tr := readingFixture(t)

	for _, raw := range []string{"999999", "nonsense", "-1", strconv.FormatInt(int64(tr.bobFeed), 10)} {
		body := rd.body("/feeds?edit=" + raw)
		if !strings.Contains(body, "Add a feed") {
			t.Errorf("?edit=%s did not fall back to the add form:\n%s", raw, body)
		}
		if strings.Contains(body, "Save changes") {
			t.Errorf("?edit=%s opened an edit form", raw)
		}
	}
}

// The address is the edit with consequences: the validators belong to the endpoint
// that issued them, and the failures belong to the address that produced them.
func TestEditFeedCorrectingTheAddressResetsPollingState(t *testing.T) {
	rd, tr := readingFixture(t)
	id := brokenFeed(t, tr, "https://news.example.com/old.xml")

	rec := rd.do(http.MethodPost, editPath(id), url.Values{
		"url":      {"https://news.example.com/new.xml"},
		"title":    {"News Publication"},
		"category": {"Newspapers"},
		"enabled":  {"true"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200\n%s", editPath(id), rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Saved") {
		t.Errorf("the page does not confirm the save:\n%s", body)
	}

	updated, err := tr.store.GetFeed(t.Context(), tr.alice, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if updated.FeedURL != "https://news.example.com/new.xml" {
		t.Errorf("feed_url = %q, want the corrected address", updated.FeedURL)
	}
	if updated.Title != "News Publication" || updated.Category != "Newspapers" {
		t.Errorf("title/category = %q/%q, want the edited values", updated.Title, updated.Category)
	}
	// An ETag from the old endpoint means nothing to the new one, and sending it
	// invites a 304 for content that has never been seen.
	if updated.ETag != "" || updated.LastModified != "" {
		t.Errorf("the validators survived the address change: etag=%q last-modified=%q",
			updated.ETag, updated.LastModified)
	}
	// The failures were the old address's, and without clearing them the feed sits
	// three short of being disabled for a fault that no longer exists.
	if updated.ConsecutiveFailures != 0 || updated.LastError != "" {
		t.Errorf("the failure history survived: %d failures, last error %q",
			updated.ConsecutiveFailures, updated.LastError)
	}
	// And it is due, because the reader has just done the thing that was meant to fix
	// it and is waiting to find out whether it worked.
	if !nextPollWithin(t, tr, id, time.Minute) {
		t.Error("the corrected feed was not queued for a poll")
	}
}

// The edits that leave the address alone must leave the poll history alone with it.
// Re-filing a feed is not a reason to forget that it has been failing for a week.
func TestEditFeedKeepsThePollHistoryWhenTheAddressIsUnchanged(t *testing.T) {
	rd, tr := readingFixture(t)
	id := brokenFeed(t, tr, "https://news.example.com/old.xml")

	rec := rd.do(http.MethodPost, editPath(id), url.Values{
		"url":      {"https://news.example.com/old.xml"},
		"title":    {"Moved Publication"},
		"category": {"Comics"},
		"enabled":  {"true"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200\n%s", editPath(id), rec.Code, rec.Body.String())
	}

	updated, err := tr.store.GetFeed(t.Context(), tr.alice, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if updated.Category != "Comics" {
		t.Errorf("category = %q, want Comics", updated.Category)
	}
	if updated.ConsecutiveFailures != 3 {
		t.Errorf("consecutive_failures = %d, want the history kept", updated.ConsecutiveFailures)
	}
	if updated.ETag == "" {
		t.Error("the validators were discarded by an edit that did not touch the address")
	}
}

// The site URL is the base relative entry links resolve against, and only an import
// ever writes it. A feed that moves to another host has taken its site with it.
func TestEditFeedDropsTheSiteURLOnlyWhenTheHostChanges(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	id, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL: "https://news.example.com/old.xml",
		SiteURL: "https://news.example.com/",
		Title:   "Moved Publication",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	// A corrected path on the same host keeps it: the site has not moved.
	rec := rd.do(http.MethodPost, editPath(id), url.Values{
		"url": {"https://news.example.com/feed/atom"}, "title": {"Moved Publication"}, "enabled": {"true"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200\n%s", editPath(id), rec.Code, rec.Body.String())
	}
	samehost, err := tr.store.GetFeed(ctx, tr.alice, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if samehost.SiteURL != "https://news.example.com/" {
		t.Errorf("site_url = %q after a path change, want it kept", samehost.SiteURL)
	}

	// A different host does not.
	rec = rd.do(http.MethodPost, editPath(id), url.Values{
		"url": {"https://newsroom.example.org/feed"}, "title": {"Moved Publication"}, "enabled": {"true"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200\n%s", editPath(id), rec.Code, rec.Body.String())
	}
	moved, err := tr.store.GetFeed(ctx, tr.alice, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if moved.SiteURL != "" {
		t.Errorf("site_url = %q after moving hosts, want it cleared so links resolve against the feed",
			moved.SiteURL)
	}
}

// Emptying the category takes the feed out of the folder. The import's upsert
// deliberately preserves a category it is not given — re-importing an OPML file must
// not unfile everything — and an edit is the opposite case.
func TestEditFeedCanEmptyTheCategory(t *testing.T) {
	rd, tr := readingFixture(t)
	id := brokenFeed(t, tr, "https://news.example.com/old.xml")

	rec := rd.do(http.MethodPost, editPath(id), url.Values{
		"url":     {"https://news.example.com/old.xml"},
		"title":   {"Moved Publication"},
		"enabled": {"true"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200\n%s", editPath(id), rec.Code, rec.Body.String())
	}

	updated, err := tr.store.GetFeed(t.Context(), tr.alice, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if updated.Category != "" {
		t.Errorf("category = %q, want it emptied", updated.Category)
	}
}

// Turning a feed off keeps its history; turning it back on clears the failure count,
// or the next single failure re-crosses the threshold and disables it again.
func TestEditFeedTurnsPollingOffAndOnAgain(t *testing.T) {
	rd, tr := readingFixture(t)
	id := brokenFeed(t, tr, "https://news.example.com/old.xml")
	address := url.Values{"url": {"https://news.example.com/old.xml"}, "title": {"Moved Publication"}}

	// Off: no "enabled" field at all, which is what an unchecked box submits.
	rec := rd.do(http.MethodPost, editPath(id), address)
	if rec.Code != http.StatusOK {
		t.Fatalf("turning it off = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	off, err := tr.store.GetFeed(t.Context(), tr.alice, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if !off.Disabled {
		t.Error("the feed is still being polled")
	}
	if off.ConsecutiveFailures != 3 || off.LastError == "" {
		t.Errorf("turning a feed off discarded why it was failing: %d failures, last error %q",
			off.ConsecutiveFailures, off.LastError)
	}

	// The list says it was turned off rather than claiming "after 0 failures", and
	// says nothing of the sort here because this one was failing.
	body := rd.body("/feeds")
	if !strings.Contains(body, "after 3 failures") {
		t.Errorf("the row does not say what it was disabled after:\n%s", body)
	}

	// On again.
	on := url.Values{}
	for k, v := range address {
		on[k] = v
	}
	on.Set("enabled", "true")

	rec = rd.do(http.MethodPost, editPath(id), on)
	if rec.Code != http.StatusOK {
		t.Fatalf("turning it on = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	revived, err := tr.store.GetFeed(t.Context(), tr.alice, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if revived.Disabled {
		t.Error("the feed was not re-enabled")
	}
	if revived.ConsecutiveFailures != 0 || revived.LastError != "" {
		t.Errorf("re-enabling left %d failures and %q, which the next failure would build on",
			revived.ConsecutiveFailures, revived.LastError)
	}
	if !nextPollWithin(t, tr, id, time.Minute) {
		t.Error("a re-enabled feed was not queued for a poll")
	}
}

// A feed turned off by hand has no failures to report, and the row must not claim
// "after 0 failures".
func TestFeedRowSaysWhyAHealthyFeedIsDisabled(t *testing.T) {
	rd, tr := readingFixture(t)

	id, _, err := tr.store.UpsertFeed(t.Context(), tr.alice, store.FeedParams{
		FeedURL: "https://quiet.example.com/feed.xml", Title: "Quiet Feed",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	rec := rd.do(http.MethodPost, editPath(id), url.Values{
		"url": {"https://quiet.example.com/feed.xml"}, "title": {"Quiet Feed"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200", editPath(id), rec.Code)
	}

	body := rd.body("/feeds")
	if strings.Contains(body, "after 0 failures") {
		t.Errorf("a feed turned off by hand is reported as having failed:\n%s", body)
	}
	if !strings.Contains(body, "turned off") {
		t.Errorf("the row does not say the feed was turned off:\n%s", body)
	}
}

// Two subscriptions to one address are indistinguishable in the list, so the
// constraint that prevents them has to reach the reader as a sentence rather than as
// a 500.
func TestEditFeedRefusesAnAddressAlreadySubscribed(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	first, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL: "https://one.example.com/feed.xml", Title: "One",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if _, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
		FeedURL: "https://two.example.com/feed.xml", Title: "Two",
	}); err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	rec := rd.do(http.MethodPost, editPath(first), url.Values{
		"url": {"https://two.example.com/feed.xml"}, "title": {"One"}, "enabled": {"true"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST %s = %d, want 409\n%s", editPath(first), rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "already uses that address") {
		t.Errorf("the page does not explain the refusal:\n%s", body)
	}
	// Named, so the refusal cannot read as the form having lost track of which
	// subscription it had open.
	if !strings.Contains(body, "Two") {
		t.Errorf("the refusal does not name the subscription holding the address:\n%s", body)
	}
	// Neither of these two has ever polled successfully, so the advice is the other
	// branch of that message: the address may be wrong in both.
	if !strings.Contains(body, "never fetched successfully either") {
		t.Errorf("the refusal does not say which of the two is working:\n%s", body)
	}
	// And the form still has what was typed, with the feed still open in it.
	if !strings.Contains(body, `action="`+editPath(first)+`"`) {
		t.Error("the rejected edit closed the form")
	}

	unchanged, err := tr.store.GetFeed(ctx, tr.alice, first)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if unchanged.FeedURL != "https://one.example.com/feed.xml" {
		t.Errorf("feed_url = %q, want it unchanged", unchanged.FeedURL)
	}
}

func TestEditFeedRejectsSomethingThatIsNotAnAddress(t *testing.T) {
	rd, tr := readingFixture(t)
	id := brokenFeed(t, tr, "https://news.example.com/old.xml")

	rec := rd.do(http.MethodPost, editPath(id), url.Values{
		"url": {"not a url at all"}, "title": {"Moved Publication"}, "enabled": {"true"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST %s = %d, want 400\n%s", editPath(id), rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "not a web address") {
		t.Errorf("the page does not say what is wrong:\n%s", body)
	}

	unchanged, err := tr.store.GetFeed(t.Context(), tr.alice, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if unchanged.FeedURL != "https://news.example.com/old.xml" {
		t.Errorf("feed_url = %q, want it unchanged", unchanged.FeedURL)
	}
}

// One reader's subscriptions are not another's, through this route as much as
// anywhere else. Not found rather than forbidden: whether a given feed id exists at
// all is itself information about somebody else's list.
func TestEditFeedIsScopedToTheReader(t *testing.T) {
	rd, tr := readingFixture(t)

	his, err := tr.store.GetFeed(t.Context(), tr.bob, tr.bobFeed)
	if err != nil {
		t.Fatalf("GetFeed(bob) = %v", err)
	}

	rec := rd.do(http.MethodPost, editPath(tr.bobFeed), url.Values{
		"url": {"https://alice.example.com/hijacked.xml"}, "title": {"Hijacked"}, "enabled": {"true"},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST %s = %d, want 404", editPath(tr.bobFeed), rec.Code)
	}

	after, err := tr.store.GetFeed(t.Context(), tr.bob, tr.bobFeed)
	if err != nil {
		t.Fatalf("GetFeed(bob) = %v", err)
	}
	if after.FeedURL != his.FeedURL || after.Title != his.Title {
		t.Errorf("Bob's feed became %q/%q", after.FeedURL, after.Title)
	}
}

// Testing an address from the edit form has to come back as the edit form. The
// hazard of one form doing both jobs is a test that quietly turns it into the add
// form, where Save creates a second subscription instead of correcting the first.
func TestTestingFromTheEditFormStaysAnEdit(t *testing.T) {
	rd, tr := fetchingFixture(t)
	site := feedSite(t)

	id, _, err := tr.store.UpsertFeed(t.Context(), tr.alice, store.FeedParams{
		FeedURL: site.URL + "/feed.xml", Title: "Example Engineering",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	rec := rd.do(http.MethodPost, "/feeds/test", url.Values{
		"url":     {site.URL + "/feed.xml"},
		"edit":    {strconv.FormatInt(int64(id), 10)},
		"enabled": {"true"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /feeds/test = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, `action="`+editPath(id)+`"`) {
		t.Errorf("testing from the edit form came back as the add form:\n%s", body)
	}
	// And it does not report the feed being edited as a subscription that already
	// exists, which is true and useless: it is the feed whose address is being
	// corrected.
	if strings.Contains(body, "already subscribed to this feed") {
		t.Errorf("the edit form warns that the feed being edited is already subscribed:\n%s", body)
	}
}

// Saving must not throw away the list the reader was looking at. These routes render
// the feeds page rather than redirecting to it, so the ordering and the filter travel
// in the form's action.
func TestEditFeedKeepsTheReadersView(t *testing.T) {
	rd, tr := readingFixture(t)
	id := brokenFeed(t, tr, "https://news.example.com/old.xml")

	rec := rd.do(http.MethodPost, editPath(id)+"?q=moved&sort=unread&dir=desc", url.Values{
		"url": {"https://news.example.com/old.xml"}, "title": {"Moved Publication"}, "enabled": {"true"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, `value="moved"`) {
		t.Errorf("the page came back without the filter:\n%s", body)
	}
	if !strings.Contains(body, `aria-sort="descending"`) {
		t.Errorf("the page came back unsorted:\n%s", body)
	}
	// Alice's other feed does not match "moved" and must not be listed.
	if strings.Contains(body, escaped("Alice's Feed")) {
		t.Error("the filter was dropped on the page rendered after a save")
	}
}
