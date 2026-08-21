package server

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// A group id belongs to the category, not to its name.
//
// This is the whole reason categories became a table. The id used to be a hash of the
// name — the protocol requires an id and there was no row to take one from — so
// **renaming a category silently reshuffled a client's folders**: the old group
// vanished and a new one appeared holding the same feeds. Clients cache folder
// membership against these numbers.
func TestAGroupIDSurvivesARename(t *testing.T) {
	before := []store.Feed{
		{ID: 1, Title: "Monkey User", Category: "Comics", CategoryID: 7},
		{ID: 2, Title: "Ars Technica", Category: "Tech", CategoryID: 9},
	}
	after := []store.Feed{
		{ID: 1, Title: "Monkey User", Category: "Webcomics", CategoryID: 7},
		{ID: 2, Title: "Ars Technica", Category: "Tech", CategoryID: 9},
	}

	idOf := func(feeds []store.Feed, title string) int64 {
		groups, _ := feverGroupsFor(feeds)
		for _, g := range groups {
			if g.Title == title {
				return g.ID
			}
		}
		return 0
	}

	renamed := idOf(before, "Comics")
	if renamed == 0 {
		t.Fatal("no group for Comics")
	}
	if got := idOf(after, "Webcomics"); got != renamed {
		t.Errorf("the renamed folder's id changed, %d then %d — a client's cached membership would break",
			renamed, got)
	}
	// And the folder that was not renamed is untouched, which an index into a sorted
	// list would have got wrong.
	if idOf(before, "Tech") != idOf(after, "Tech") {
		t.Error("renaming one category moved another category's id")
	}
}

// The protocol says a group id is a positive integer, and reserves 0 and -1 for its
// own super groups. Staying inside a signed 32-bit range is for the client written in
// 2013 that put these in an INTEGER column.
//
// This used to be enforced by the hash's modulus. Now it is a property of the ids the
// database hands out, which is worth asserting rather than assuming: nothing in the
// schema bounds a bigserial, and the check is what would notice if these ever came
// from somewhere else.
func TestGroupIDsStayInTheProtocolsRange(t *testing.T) {
	const maxInt32 = 1<<31 - 1

	feeds := []store.Feed{
		{ID: 1, Title: "A", Category: "Comics", CategoryID: 1},
		{ID: 2, Title: "B", Category: "Tech", CategoryID: 2},
		{ID: 3, Title: "C", Category: "Guns", CategoryID: 4096},
	}

	groups, _ := feverGroupsFor(feeds)
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3", len(groups))
	}
	seen := make(map[int64]string, len(groups))
	for _, g := range groups {
		if g.ID < 1 || g.ID > maxInt32 {
			t.Errorf("the id for %q is %d, outside [1, %d]", g.Title, g.ID, maxInt32)
		}
		if other, taken := seen[g.ID]; taken {
			t.Errorf("%q and %q were both assigned id %d", g.Title, other, g.ID)
		}
		seen[g.ID] = g.Title
	}
}

// The reverse lookup has to agree with the forward one, or marking a group read marks
// the wrong folder.
func TestAGroupIDResolvesBackToItsCategory(t *testing.T) {
	feeds := []store.Feed{
		{ID: 1, Title: "A", Category: "Comics", CategoryID: 7},
		{ID: 2, Title: "B", Category: "Tech", CategoryID: 9},
		{ID: 3, Title: "C"},
	}

	groups, _ := feverGroupsFor(feeds)
	for _, g := range groups {
		name, ok := feverCategoryFor(feeds, g.ID)
		if !ok || name != g.Title {
			t.Errorf("group %d (%q) resolved back to %q, ok=%v", g.ID, g.Title, name, ok)
		}
	}

	// An id this reader has no folder for resolves to nothing rather than to the
	// first folder it happens to find.
	if name, ok := feverCategoryFor(feeds, 999999); ok {
		t.Errorf("an unknown group id resolved to %q", name)
	}
}

// The nameless bucket is not a folder and must not be reachable as one: a group id
// resolving to "no category" would mark the wrong articles read.
//
// It has no row by design — "no folder" is the absence of a category, not a category
// standing for absence — so there is no id it could be reached by. This asserts that
// an unfiled feed contributes neither a group nor a resolvable id, including when the
// database has handed it a zero.
func TestTheNamelessCategoryIsNotAGroup(t *testing.T) {
	feeds := []store.Feed{
		{ID: 1, Title: "Unfiled"},
		{ID: 2, Title: "Also unfiled", Category: "", CategoryID: 0},
		{ID: 3, Title: "Filed", Category: "Tech", CategoryID: 5},
	}

	groups, _ := feverGroupsFor(feeds)
	for _, g := range groups {
		if g.Title == "" {
			t.Errorf("the nameless bucket was offered as group %d", g.ID)
		}
	}
	if len(groups) != 1 {
		t.Errorf("got %d groups, want only Tech: %+v", len(groups), groups)
	}

	// Zero is what an unfiled feed's category id scans as, and it must not resolve.
	if name, ok := feverCategoryFor(feeds, 0); ok {
		t.Errorf("group id 0 resolved to %q; the protocol reserves it and the bucket has no id", name)
	}

	// A name with no id cannot come from a read — both sides come from the same join,
	// so a feed has either or neither. It can come from a caller assembling a Feed by
	// hand, which is how the fixture in TestGroupsAreBuiltFromTheSubscriptions was
	// written before this change. **The protocol reserves 0**, so emitting it as a
	// group is worse than emitting nothing: a client would ask to mark a super group
	// read.
	inconsistent := []store.Feed{{ID: 1, Title: "Odd", Category: "Tech", CategoryID: 0}}
	for _, g := range mustGroups(t, inconsistent) {
		if g.ID == 0 {
			t.Errorf("a feed with a category name and no id produced group id 0 (%q), which the protocol reserves", g.Title)
		}
	}
}

// mustGroups is feverGroupsFor with the membership half discarded, for the tests that
// only care what folders were offered.
func mustGroups(t *testing.T, feeds []store.Feed) []feverGroup {
	t.Helper()
	groups, _ := feverGroupsFor(feeds)
	return groups
}

func TestGroupsAreBuiltFromTheSubscriptions(t *testing.T) {
	// Both the name and the id, because that is what a read produces: they come from
	// the same join, so a feed has either both or neither. A name without an id is
	// deliberately not a group — it would mean a folder nothing could be marked
	// against.
	feeds := []store.Feed{
		{ID: 1, Title: "Monkey User", Category: "Comics", CategoryID: 3},
		{ID: 2, Title: "Ars Technica", Category: "Tech", CategoryID: 8},
		{ID: 3, Title: "Commit Strip", Category: "Comics", CategoryID: 3},
		{ID: 4, Title: "Something Unfiled"},
	}

	groups, byGroup := feverGroupsFor(feeds)

	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 (Comics and Tech, not the unfiled feed): %+v", len(groups), groups)
	}
	// Sorted by title, so a client's folder list has a stable order rather than the
	// order the subscriptions came back in.
	if groups[0].Title != "Comics" || groups[1].Title != "Tech" {
		t.Errorf("groups are not sorted by title: %+v", groups)
	}

	membership := make(map[string]string, len(byGroup))
	for _, g := range byGroup {
		for _, group := range groups {
			if group.ID == g.GroupID {
				membership[group.Title] = g.FeedIDs
			}
		}
	}

	// Comma-separated inside a string, which is the protocol's own choice.
	if got := membership["Comics"]; got != "1,3" {
		t.Errorf("the Comics group holds %q, want \"1,3\"", got)
	}
	if got := membership["Tech"]; got != "2" {
		t.Errorf("the Tech group holds %q, want \"2\"", got)
	}

	// Feed 4 is in no group at all. Fever has no ungrouped concept and clients cope;
	// inventing an "Uncategorized" folder would put something in somebody's reader
	// that is not in their archive.
	for _, g := range byGroup {
		if strings.Contains(g.FeedIDs, "4") {
			t.Errorf("the unfiled feed was put in a group: %+v", g)
		}
	}
}

func TestIDListsUseTheProtocolsShape(t *testing.T) {
	if got := feverIDList([]store.ArticleID{3, 1, 4}); got != "3,1,4" {
		t.Errorf("feverIDList() = %q", got)
	}
	// Empty rather than "0" or "null": the member has to be present and say nothing.
	if got := feverIDList(nil); got != "" {
		t.Errorf("feverIDList(nil) = %q, want the empty string", got)
	}
}

func TestWithIDsSkipsJunkAndStopsAtTheProtocolsCap(t *testing.T) {
	// A reading request, so a bad entry is skipped rather than failing the whole
	// call — the opposite of the scroll-marking endpoint, which writes.
	got := feverIDs("7, 9,not-a-number,,11,-4,0")
	want := []store.ArticleID{7, 9, 11}
	if !slices.Equal(got, want) {
		t.Errorf("feverIDs() = %v, want %v", got, want)
	}

	var many strings.Builder
	for i := 1; i <= store.FeverItemLimit+20; i++ {
		fmt.Fprintf(&many, "%d,", i)
	}
	if n := len(feverIDs(many.String())); n != store.FeverItemLimit {
		t.Errorf("feverIDs() returned %d ids, want the protocol's cap of %d",
			n, store.FeverItemLimit)
	}
}

// The base URL is what makes an image reference absolute, and the scheme cannot come
// from the request when TLS is terminated upstream — which is every deployment behind
// an Ingress.
func TestTheBaseURLTakesTheHostFromTheRequest(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://tome.example.org/fever/", nil)

	if got := feverBaseURL(req, true); got != "https://tome.example.org" {
		t.Errorf("feverBaseURL(secure) = %q", got)
	}
	// A deployment that turned COOKIE_SECURE off has said it is reached over plain
	// HTTP, and its image URLs have to say the same or they point somewhere that does
	// not answer.
	if got := feverBaseURL(req, false); got != "http://tome.example.org" {
		t.Errorf("feverBaseURL(insecure) = %q", got)
	}

	// A request that really did arrive over TLS says so regardless of configuration.
	req.TLS = &tls.ConnectionState{}
	if got := feverBaseURL(req, false); got != "https://tome.example.org" {
		t.Errorf("feverBaseURL(TLS request) = %q", got)
	}
}

func TestAMissingBodySaysSoAndOnlyLinksSchemesWeIssued(t *testing.T) {
	got := feverMissingBody("https://example.com/gone")
	if !strings.Contains(got, `href="https://example.com/gone"`) {
		t.Errorf("the missing-body note does not link the original: %q", got)
	}

	// Canonicalization never produces anything but http or https, so this is belt and
	// braces — but the thing it guards against is a scheme this application did not
	// choose ending up in an href a mobile client will render.
	for _, hostile := range []string{
		"javascript:alert(1)", "data:text/html,<script>alert(1)</script>", "",
	} {
		got := feverMissingBody(hostile)
		if strings.Contains(got, "href") {
			t.Errorf("feverMissingBody(%q) emitted a link: %q", hostile, got)
		}
	}
}
