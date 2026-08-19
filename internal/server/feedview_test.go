package server

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// An internal test, deliberately. Ordering and filtering are decisions about a
// slice, and testing them through a rendered page would mean a database, a session
// and an assertion about the order two titles appear in some HTML — which is a slow
// way to find out whether a comparison function is right. The integration tests
// alongside cover that the page actually uses this.

// row builds one list row. The zero value is a healthy, uncategorized feed that has
// never succeeded, and every case below says only what it cares about.
func row(title, category string, unread int64, opts ...func(*feedRow)) feedRow {
	r := feedRow{Unread: unread}
	r.Title = title
	r.Category = category
	r.FeedURL = "https://" + strings.ToLower(strings.ReplaceAll(title, " ", "-")) + ".example.com/feed"
	for _, opt := range opts {
		opt(&r)
	}
	return r
}

func succeeded(at time.Time) func(*feedRow) {
	return func(r *feedRow) { r.LastSuccessAt = &at }
}

func failing(n int) func(*feedRow) {
	return func(r *feedRow) { r.ConsecutiveFailures = n }
}

func disabled(after int) func(*feedRow) {
	return func(r *feedRow) { r.Disabled, r.ConsecutiveFailures = true, after }
}

func names(rows []feedRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.displayName())
	}
	return out
}

func TestSortRows(t *testing.T) {
	recent := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	older := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	// As the store returns them: category, then title. Every case starts from this,
	// which is what makes the stability assertions meaningful.
	stored := func() []feedRow {
		return []feedRow{
			row("Comic Strip", "Comics", 12, succeeded(older), failing(3)),
			row("Daily Panel", "Comics", 0, succeeded(recent)),
			row("Kernel Notes", "Tech", 4, disabled(20)),
			row("zebra weekly", "Tech", 7, succeeded(recent)),
			row("Anonymous Wire", "", 0),
		}
	}

	for _, c := range []struct {
		name string
		view feedView
		want []string
	}{
		{
			name: "no sort leaves the store's order",
			view: feedView{},
			want: []string{"Comic Strip", "Daily Panel", "Kernel Notes", "zebra weekly", "Anonymous Wire"},
		},
		{
			// Case-folded, so a lowercased title is not filed after Z.
			name: "by title",
			view: feedView{Sort: sortTitle},
			want: []string{"Anonymous Wire", "Comic Strip", "Daily Panel", "Kernel Notes", "zebra weekly"},
		},
		{
			name: "by title, reversed",
			view: feedView{Sort: sortTitle, Desc: true},
			want: []string{"zebra weekly", "Kernel Notes", "Daily Panel", "Comic Strip", "Anonymous Wire"},
		},
		{
			// Uncategorized last, and feeds sharing a category keep the store's order
			// within it rather than being shuffled.
			name: "by category",
			view: feedView{Sort: sortCategory},
			want: []string{"Comic Strip", "Daily Panel", "Kernel Notes", "zebra weekly", "Anonymous Wire"},
		},
		{
			name: "by unread, most first",
			view: feedView{Sort: sortUnread, Desc: true},
			want: []string{"Comic Strip", "zebra weekly", "Kernel Notes", "Daily Panel", "Anonymous Wire"},
		},
		{
			// Never-succeeded is the oldest thing there is, because nothing has ever
			// arrived from it.
			name: "by last success, oldest first",
			view: feedView{Sort: sortLast},
			want: []string{"Kernel Notes", "Anonymous Wire", "Comic Strip", "Daily Panel", "zebra weekly"},
		},
		{
			name: "by health, worst first",
			view: feedView{Sort: sortHealth, Desc: true},
			want: []string{"Kernel Notes", "Comic Strip", "Daily Panel", "zebra weekly", "Anonymous Wire"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows := stored()
			c.view.sortRows(rows)

			got := names(rows)
			if len(got) != len(c.want) {
				t.Fatalf("sorted %d rows, want %d", len(got), len(c.want))
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("sorted order is\n  %v\nwant\n  %v", got, c.want)
				}
			}
		})
	}
}

// Reversing a column must not reverse the rows it cannot separate. Two feeds with
// the same unread count belong in the order the list already had them, or the table
// looks shuffled every time somebody clicks a heading twice.
func TestSortRowsKeepsTiesStableInBothDirections(t *testing.T) {
	stored := []feedRow{row("Aardvark", "", 0), row("Beaver", "", 0), row("Cheetah", "", 0)}

	for _, desc := range []bool{false, true} {
		rows := append([]feedRow(nil), stored...)
		feedView{Sort: sortUnread, Desc: desc}.sortRows(rows)

		got := names(rows)
		for i, want := range []string{"Aardvark", "Beaver", "Cheetah"} {
			if got[i] != want {
				t.Fatalf("desc=%v reordered tied rows to %v", desc, got)
			}
		}
	}
}

func TestFilterMatches(t *testing.T) {
	healthy := row("Daily Panel", "Comics", 0)
	broken := row("Comic Strip", "Comics", 0, failing(3))
	off := row("Kernel Notes", "Tech", 0, disabled(20))

	for _, c := range []struct {
		name string
		view feedView
		want []feedRow
	}{
		{"no filter keeps everything", feedView{}, []feedRow{healthy, broken, off}},
		{"healthy only", feedView{Health: healthOK}, []feedRow{healthy}},
		// A disabled feed is its own state rather than the worst end of failing:
		// asking what is failing is asking what is going wrong now.
		{"failing only excludes disabled", feedView{Health: healthFailing}, []feedRow{broken}},
		{"disabled only", feedView{Health: healthDisabled}, []feedRow{off}},
		{"matches a title, case-insensitively", feedView{Query: "daily"}, []feedRow{healthy}},
		{"matches a category", feedView{Query: "comics"}, []feedRow{healthy, broken}},
		{"matches an address", feedView{Query: "kernel-notes.example.com"}, []feedRow{off}},
		{"filters combine", feedView{Query: "comic", Health: healthOK}, []feedRow{healthy}},
		{"matching nothing keeps nothing", feedView{Query: "no such feed"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []feedRow
			for _, r := range []feedRow{healthy, broken, off} {
				if c.view.matches(r) {
					got = append(got, r)
				}
			}
			if len(got) != len(c.want) {
				t.Fatalf("kept %v, want %v", names(got), names(c.want))
			}
			for i := range got {
				if got[i].Title != c.want[i].Title {
					t.Fatalf("kept %v, want %v", names(got), names(c.want))
				}
			}
		})
	}
}

func TestFeedViewFrom(t *testing.T) {
	for _, c := range []struct {
		query string
		want  feedView
	}{
		{"", feedView{}},
		{"q=comics&health=failing", feedView{Query: "comics", Health: healthFailing}},
		{"q=+padded+", feedView{Query: "padded"}},
		// A key with no direction takes the useful end of its own column.
		{"sort=title", feedView{Sort: sortTitle}},
		{"sort=unread", feedView{Sort: sortUnread, Desc: true}},
		{"sort=health", feedView{Sort: sortHealth, Desc: true}},
		{"sort=unread&dir=asc", feedView{Sort: sortUnread}},
		{"sort=title&dir=desc", feedView{Sort: sortTitle, Desc: true}},
		// Nonsense typed into the URL shows the list rather than an error.
		{"sort=publisher&dir=desc", feedView{}},
		{"health=perfect", feedView{}},
		// A direction with nothing to order is not an order.
		{"dir=desc", feedView{}},
	} {
		t.Run(c.query, func(t *testing.T) {
			values, err := url.ParseQuery(c.query)
			if err != nil {
				t.Fatalf("ParseQuery(%q) = %v", c.query, err)
			}
			if got := feedViewFrom(values); got != c.want {
				t.Errorf("feedViewFrom(%q) = %+v, want %+v", c.query, got, c.want)
			}
		})
	}
}

// The links are how the view survives a click. Every one of them has to carry the
// rest of it, or sorting silently clears a filter and editing a row drops the
// reader back at the top of an unfiltered list.
func TestFeedViewLinks(t *testing.T) {
	plain := feedView{}
	filtered := feedView{Query: "comics", Health: healthFailing, Sort: sortUnread, Desc: true}

	if got := plain.SortHref("title"); got != "/feeds?dir=asc&sort=title" {
		t.Errorf("first click on Feed = %q", got)
	}
	// Clicking the sorted column reverses it; clicking another takes that column's
	// own first direction.
	if got := filtered.SortHref("unread"); !strings.Contains(got, "dir=asc") {
		t.Errorf("clicking the sorted column did not reverse it: %q", got)
	}
	if got := filtered.SortHref("title"); !strings.Contains(got, "dir=asc") {
		t.Errorf("clicking an unsorted text column = %q, want ascending", got)
	}
	if got := filtered.SortHref("health"); !strings.Contains(got, "dir=desc") {
		t.Errorf("clicking Health = %q, want descending", got)
	}
	for _, href := range []string{
		filtered.SortHref("title"),
		filtered.EditHref(store.FeedID(12)),
		filtered.CancelHref(),
	} {
		if !strings.Contains(href, "q=comics") || !strings.Contains(href, "health=failing") {
			t.Errorf("%q does not carry the filter", href)
		}
	}
	if got := filtered.EditHref(store.FeedID(12)); !strings.Contains(got, "edit=12") {
		t.Errorf("EditHref = %q, want the feed's id", got)
	}

	// Clearing drops the filter and keeps the ordering: somebody clearing a search
	// has not asked to be re-sorted.
	cleared := filtered.ClearHref()
	if strings.Contains(cleared, "q=") || strings.Contains(cleared, "health=") {
		t.Errorf("ClearHref = %q, want no filter", cleared)
	}
	if !strings.Contains(cleared, "sort=unread") {
		t.Errorf("ClearHref = %q, want the ordering kept", cleared)
	}

	// An unsorted, unfiltered list is a plain URL.
	if got := plain.CancelHref(); got != "/feeds" {
		t.Errorf("CancelHref with no view = %q, want /feeds", got)
	}

	if got := filtered.AriaSort("unread"); got != "descending" {
		t.Errorf("AriaSort of the sorted column = %q", got)
	}
	if got := filtered.AriaSort("title"); got != "" {
		t.Errorf("AriaSort of an unsorted column = %q, want empty", got)
	}
}

// The form's actions have to carry the view for the same reason the links do: these
// routes render the list rather than redirecting to it, and a POST brings no query
// string of its own.
func TestFeedsPageFormActions(t *testing.T) {
	page := feedsPage{View: feedView{Query: "comics"}}

	if got := page.FormAction(); got != "/feeds/add?q=comics" {
		t.Errorf("the blank form posts to %q", got)
	}
	if got := page.TestAction(); got != "/feeds/test?q=comics" {
		t.Errorf("Test posts to %q", got)
	}

	page.Add = &addFeedOutcome{EditingID: store.FeedID(7)}
	if got := page.FormAction(); got != "/feeds/7/edit?q=comics" {
		t.Errorf("the edit form posts to %q", got)
	}
	if page.Editing() != store.FeedID(7) {
		t.Errorf("Editing() = %d, want 7", page.Editing())
	}
}
