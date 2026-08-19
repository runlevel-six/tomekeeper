package server

import (
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The token grammar, tested without a database because that is what it is: string
// handling with one rule that matters — the variable part goes last, because a
// category is a folder name from somebody else's reader and can contain anything.

func TestUnreadCategoryTokenRoundTrips(t *testing.T) {
	s := &Server{}

	for _, name := range []string{
		"Comics",
		"", // the nameless bucket is a real category
		"Cooking: BBQ",
		"Tech/Go",
		"News & Views",
		"unread-category:not-a-token",
	} {
		spec := s.unreadCategorySpec(name)

		back, ok := s.streamSpecFor(t.Context(), store.UserID(1), spec.Token)
		if !ok {
			t.Fatalf("the token for %q did not resolve back to a list: %q", name, spec.Token)
		}
		if !back.Query.Categorized || back.Query.Category != name {
			t.Errorf("%q came back as category %q (categorized=%v)",
				name, back.Query.Category, back.Query.Categorized)
		}
		if !back.Query.UnreadOnly {
			t.Errorf("%q came back without the unread filter", name)
		}
		if back.Token != spec.Token || back.Path != spec.Path || back.Heading != spec.Heading {
			t.Errorf("%q resolved to a different list: %+v", name, back)
		}
	}
}

// The two category tokens must not be confused for one another, in either direction:
// one is a folder's whole archive and the other is what is new in it.
func TestCategoryTokensAreDistinct(t *testing.T) {
	s := &Server{}
	ctx := t.Context()

	unread, ok := s.streamSpecFor(ctx, 1, streamUnreadCategory+"Comics")
	if !ok {
		t.Fatal("the unread-in-category token did not resolve")
	}
	if !unread.Query.UnreadOnly {
		t.Error("unread-category resolved to the whole category")
	}

	whole, ok := s.streamSpecFor(ctx, 1, streamCategory+"Comics")
	if !ok {
		t.Fatal("the category token did not resolve")
	}
	if whole.Query.UnreadOnly {
		t.Error("the category token resolved to the unread list")
	}
	if whole.Token == unread.Token {
		t.Error("the two lists share a token")
	}

	// A category *named* like the other prefix must still be itself.
	odd, ok := s.streamSpecFor(ctx, 1, streamCategory+"unread-category:x")
	if !ok || odd.Query.UnreadOnly || odd.Query.Category != "unread-category:x" {
		t.Errorf("a category named after the other prefix resolved wrongly: %+v", odd)
	}
}

// The heading doubles as the label on the way back from an article, so it has to read
// as a place. "Unread in No category" does not, which is why the nameless bucket gets
// its own phrasing.
func TestUnreadCategoryHeadingReadsAsAPlace(t *testing.T) {
	if got := unreadCategoryHeading("Comics"); got != "Unread in Comics" {
		t.Errorf("heading = %q", got)
	}
	if got := unreadCategoryHeading(""); got != "Unread with no category" {
		t.Errorf("heading for the nameless category = %q", got)
	}
}

// Which lists offer the control, and — more importantly — which do not.
func TestOnlyTheSpanningListsAreNarrowable(t *testing.T) {
	s := &Server{}

	for _, c := range []struct {
		name string
		spec streamSpec
		want bool
	}{
		{"unread", s.unreadSpec(), true},
		{"everything", s.allSpec(), true},
		{"one category", s.categorySpec("Comics"), true},
		{"unread in one category", s.unreadCategorySpec("Comics"), true},
		// A feed is already inside one category; a tag deliberately crosses them; the
		// reading list holds pages that came from no feed and so have no category at
		// all; starring is a per-article decision.
		{"one feed", s.feedSpec(store.Feed{ID: 1, Title: "F"}), false},
		{"one tag", s.tagSpec(store.TagID(1)), false},
		{"saved", s.savedSpec(), false},
		{"starred", s.starredSpec(), false},
	} {
		if c.spec.Narrowable != c.want {
			t.Errorf("%s: Narrowable = %v, want %v", c.name, c.spec.Narrowable, c.want)
		}
	}
}

// Where the control points is the whole design decision: narrowing the unread list
// stays on the unread list, and narrowing everything hands the reader the category's
// own address rather than a second address for the same articles.
func TestCategoryPillDestinations(t *testing.T) {
	for _, c := range []struct {
		name      string
		spec      streamSpec
		wantOut   string
		wantFirst string
	}{
		{"from unread", (&Server{}).unreadSpec(), "/", "/?category=Comics"},
		{"from everything", (&Server{}).allSpec(), "/all", "/categories?name=Comics"},
		{"from a category", (&Server{}).categorySpec("Tech"), "/all", "/categories?name=Comics"},
		{"from unread in a category", (&Server{}).unreadCategorySpec("Tech"), "/", "/?category=Comics"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pills := categoryPillsFor(c.spec, []string{"Comics", "Tech", ""})

			if pills[0].Href != c.wantOut {
				t.Errorf("the way out points at %q, want %q", pills[0].Href, c.wantOut)
			}
			if pills[1].Href != c.wantFirst {
				t.Errorf("the first category points at %q, want %q", pills[1].Href, c.wantFirst)
			}
			// Exactly one entry is current, and it is the one the list is showing.
			var current []string
			for _, p := range pills {
				if p.Current {
					current = append(current, p.Label)
				}
			}
			if len(current) != 1 {
				t.Fatalf("%d entries claim to be current: %v", len(current), current)
			}
			want := "All categories"
			if c.spec.Query.Categorized {
				want = categoryHeading(c.spec.Query.Category)
			}
			if current[0] != want {
				t.Errorf("current entry is %q, want %q", current[0], want)
			}
			// The nameless bucket is selectable and labeled, not dropped.
			last := pills[len(pills)-1]
			if last.Label != UncategorizedHeading {
				t.Errorf("the nameless category is labeled %q", last.Label)
			}
			if !strings.HasSuffix(last.Href, "category=") && !strings.HasSuffix(last.Href, "name=") {
				t.Errorf("the nameless category points at %q, which does not select it", last.Href)
			}
		})
	}
}

// A control that can only select what is already selected is furniture, so it is not
// drawn: an archive whose feeds are all unfiled has exactly one bucket.
func TestNoCategoryControlWithNothingToChooseBetween(t *testing.T) {
	spec := (&Server{}).unreadSpec()

	for _, names := range [][]string{nil, {""}, {"Comics"}} {
		if pills := categoryPillsFor(spec, names); pills != nil {
			t.Errorf("categories %v produced a control with %d entries", names, len(pills))
		}
	}
	if pills := categoryPillsFor(spec, []string{"Comics", ""}); len(pills) != 3 {
		t.Errorf("two buckets produced %d entries, want the way out plus both", len(pills))
	}
}

// A list that takes no control must not even ask the store for the names.
func TestNarrowableIsCheckedBeforeTheQuery(t *testing.T) {
	// A nil store would panic if it were consulted, which is the assertion.
	s := &Server{}

	if pills := s.categoryPills(t.Context(), store.UserID(1), s.starredSpec()); pills != nil {
		t.Errorf("the starred list produced a category control: %v", pills)
	}
}
