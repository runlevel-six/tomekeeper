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

// Group ids are derived from category names, so the property that matters is that
// they do not move. A client caches its folder list and each folder's membership
// against these numbers.
func TestGroupIDsAreStable(t *testing.T) {
	names := []string{"Tech", "Comics", "Kubernetes"}

	first := feverGroupIDs(names)

	// The same names in a different order.
	shuffled := feverGroupIDs([]string{"Kubernetes", "Tech", "Comics"})
	for _, name := range names {
		if first[name] != shuffled[name] {
			t.Errorf("the id for %q depends on the order the categories arrive in: %d then %d",
				name, first[name], shuffled[name])
		}
	}

	// And with an unrelated category added, which is the case an index into a sorted
	// list would get wrong: subscribing to something new would renumber the folders
	// somebody already had.
	grown := feverGroupIDs(append(slices.Clone(names), "Aardvarks"))
	for _, name := range names {
		if first[name] != grown[name] {
			t.Errorf("adding a category changed the id for %q: %d then %d",
				name, first[name], grown[name])
		}
	}
}

// The protocol says a group id is a positive integer, and reserves 0 and -1 for its
// own super groups. Staying inside a signed 32-bit range is for the client written in
// 2013 that put these in an INTEGER column.
func TestGroupIDsStayInTheProtocolsRange(t *testing.T) {
	names := make([]string, 0, 2000)
	for i := range 2000 {
		names = append(names, fmt.Sprintf("category %d", i))
	}

	ids := feverGroupIDs(names)
	if len(ids) != len(names) {
		t.Fatalf("got %d ids for %d categories", len(ids), len(names))
	}

	seen := make(map[int64]string, len(ids))
	for name, id := range ids {
		if id <= 0 || id > feverGroupMax {
			t.Errorf("the id for %q is %d, outside [1, %d]", name, id, feverGroupMax)
		}
		if other, dup := seen[id]; dup {
			t.Errorf("%q and %q were both assigned id %d", name, other, id)
		}
		seen[id] = name
	}
}

// A real collision cannot be provoked in a test at this width, so what is checked
// here is the mechanism the resolution depends on: a retry has to land somewhere
// else. If the attempt counter stopped affecting the hash, the loop in feverGroupIDs
// would spin forever rather than resolve anything, and no other test would say so.
func TestARetriedGroupHashMoves(t *testing.T) {
	const name = "Comics"

	first := feverGroupHash(name, 0)
	for attempt := 1; attempt < 5; attempt++ {
		if feverGroupHash(name, attempt) == first {
			t.Errorf("attempt %d hashes %q to the same id as the first try", attempt, name)
		}
	}
}

// The reverse lookup has to agree with the forward one, or marking a group read
// would mark a different folder.
func TestAGroupIDResolvesBackToItsCategory(t *testing.T) {
	names := []string{"Tech", "Comics", "Kubernetes"}
	ids := feverGroupIDs(names)

	for _, want := range names {
		got, ok := feverCategoryFor(names, ids[want])
		if !ok {
			t.Errorf("the id for %q resolves to no category", want)
			continue
		}
		if got != want {
			t.Errorf("the id for %q resolves to %q", want, got)
		}
	}

	if _, ok := feverCategoryFor(names, 999999); ok {
		t.Error("an id belonging to no category resolved to one")
	}
}

// The nameless bucket is not a group, and must not become one by the back door.
//
// ListCategoryNames includes the empty string for feeds filed nowhere. The groups
// response leaves it out, so the reverse lookup has to leave it out too — otherwise
// a group id could resolve to "no category" and a mark-group would write to the
// wrong set of articles.
func TestTheNamelessCategoryIsNotAGroup(t *testing.T) {
	names := []string{"", "Tech"}

	// Whatever the empty name would hash to, no id may resolve to it.
	for _, id := range feverGroupIDs([]string{""}) {
		if got, ok := feverCategoryFor(names, id); ok && got == "" {
			t.Errorf("id %d resolved to the nameless category", id)
		}
	}
}

func TestGroupsAreBuiltFromTheSubscriptions(t *testing.T) {
	feeds := []store.Feed{
		{ID: 1, Title: "Monkey User", Category: "Comics"},
		{ID: 2, Title: "Ars Technica", Category: "Tech"},
		{ID: 3, Title: "Commit Strip", Category: "Comics"},
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
