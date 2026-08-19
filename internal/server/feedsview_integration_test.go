package server_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The feeds page as a list: what it draws, in what order, and how much of it. The
// ordering and filtering themselves are covered as functions in feedview_test.go;
// these are about the page using them, and about the two things a rendered list can
// get wrong that a comparison function cannot — which rows are drawn and where the
// controls sit.

// listFixture gives Alice four feeds with distinguishable names, categories, unread
// counts and health, and returns their ids by title.
func listFixture(t *testing.T) (*reader, twoReadersHTTP, map[string]store.FeedID) {
	t.Helper()

	rd, tr := readingFixture(t)
	ctx := t.Context()
	ids := map[string]store.FeedID{}

	for _, f := range []struct {
		title, url, category string
	}{
		{"Comic Strip", "https://comics.example.com/strip.xml", "Comics"},
		{"Daily Panel", "https://comics.example.com/panel.xml", "Comics"},
		{"Kernel Notes", "https://tech.example.com/kernel.xml", "Tech"},
		{"Wire Service", "https://news.example.com/wire.xml", ""},
	} {
		id, _, err := tr.store.UpsertFeed(ctx, tr.alice, store.FeedParams{
			FeedURL: f.url, Title: f.title, Category: f.category,
		})
		if err != nil {
			t.Fatalf("UpsertFeed(%s) = %v", f.title, err)
		}
		ids[f.title] = id
	}

	// One failing, one disabled, so the health filter has something to select.
	if _, err := tr.store.RecordPollFailure(ctx, tr.alice, ids["Comic Strip"],
		"HTTP 500", time.Hour, 20); err != nil {
		t.Fatalf("RecordPollFailure() = %v", err)
	}
	if err := tr.store.SetFeedDisabled(ctx, tr.alice, ids["Kernel Notes"], true); err != nil {
		t.Fatalf("SetFeedDisabled() = %v", err)
	}

	return rd, tr, ids
}

// order is the titles in the order the table lists them.
func order(t *testing.T, body string, titles ...string) []string {
	t.Helper()

	type at struct {
		title string
		index int
	}
	var found []at
	for _, title := range titles {
		if i := strings.Index(body, `>`+escaped(title)+`</a>`); i >= 0 {
			found = append(found, at{title, i})
		}
	}
	// Insertion order by position, which is all this needs — the lists are five long.
	for i := 1; i < len(found); i++ {
		for j := i; j > 0 && found[j].index < found[j-1].index; j-- {
			found[j], found[j-1] = found[j-1], found[j]
		}
	}

	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.title)
	}
	return out
}

func TestFeedsPageSortsByAColumnOnRequest(t *testing.T) {
	rd, _, _ := listFixture(t)

	// Every heading is a link, which is the only way to sort without JavaScript.
	body := rd.body("/feeds")
	for _, want := range []string{
		`href="/feeds?dir=asc&amp;sort=title"`,
		`href="/feeds?dir=desc&amp;sort=unread"`,
		`href="/feeds?dir=desc&amp;sort=health"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the table has no sort link %s:\n%s", want, body)
		}
	}

	titles := []string{"Alice's Feed", "Comic Strip", "Daily Panel", "Kernel Notes", "Wire Service"}

	descending := rd.body("/feeds?sort=title&dir=desc")
	got := order(t, descending, titles...)
	for i, want := range []string{"Wire Service", "Kernel Notes", "Daily Panel", "Comic Strip", "Alice's Feed"} {
		if i >= len(got) || got[i] != want {
			t.Fatalf("sorted by title descending, the list reads %v", got)
		}
	}

	// The sorted column is marked, once, and only that column.
	if n := strings.Count(descending, `aria-sort=`); n != 1 {
		t.Errorf("%d columns claim to be sorted, want 1", n)
	}
	if !strings.Contains(descending, `aria-sort="descending"`) {
		t.Error("the sorted column does not say which way")
	}
	// And clicking it again reverses it rather than sorting it the same way twice.
	if !strings.Contains(descending, `href="/feeds?dir=asc&amp;sort=title"`) {
		t.Error("the sorted heading does not offer to reverse itself")
	}
}

func TestFeedsPageFiltersTheList(t *testing.T) {
	rd, _, _ := listFixture(t)

	comics := rd.body("/feeds?q=comics")
	for _, want := range []string{"Comic Strip", "Daily Panel"} {
		if !strings.Contains(comics, escaped(want)) {
			t.Errorf("filtering on comics dropped %s:\n%s", want, comics)
		}
	}
	if strings.Contains(comics, escaped("Wire Service")) {
		t.Error("filtering on comics kept a feed that does not match")
	}
	// A filtered list says what it is a subset of, and offers the way back.
	if !strings.Contains(comics, "of 5 feeds") {
		t.Errorf("the filtered list does not say how much it is hiding:\n%s", comics)
	}
	if !strings.Contains(comics, `href="/feeds"`) {
		t.Error("the filtered list offers no way to clear the filter")
	}

	// The health filter treats disabled as its own state rather than the worst end of
	// failing, which is why these two select different feeds.
	failing := rd.body("/feeds?health=failing")
	if !strings.Contains(failing, escaped("Comic Strip")) {
		t.Errorf("the failing feed is missing from the failing-only list:\n%s", failing)
	}
	if strings.Contains(failing, escaped("Kernel Notes")) {
		t.Error("a disabled feed is listed as failing")
	}

	off := rd.body("/feeds?health=disabled")
	if !strings.Contains(off, escaped("Kernel Notes")) {
		t.Errorf("the disabled feed is missing from the disabled-only list:\n%s", off)
	}
	if strings.Contains(off, escaped("Daily Panel")) {
		t.Error("a healthy feed is listed as disabled")
	}

	// A filter that matches nothing says so, rather than looking like an archive with
	// no subscriptions in it.
	empty := rd.body("/feeds?q=no+such+publication")
	if !strings.Contains(empty, "No feed matches that") {
		t.Errorf("an empty filter result is not explained:\n%s", empty)
	}
	if strings.Contains(empty, "No subscriptions yet") {
		t.Error("an empty filter result claims there are no subscriptions")
	}
}

// The banner counts the whole archive, not the filtered view: "one feed is failing"
// is a fact about the subscriptions, and hiding it behind a search would be a way to
// stop being told about a slow puncture.
func TestFeedsPageCountsFailuresAcrossEverything(t *testing.T) {
	rd, _, _ := listFixture(t)

	body := rd.body("/feeds?q=wire")
	if !strings.Contains(body, "2 feeds are failing") {
		t.Errorf("the filtered page does not report every failing feed:\n%s", body)
	}
}

// Sorting and filtering have to survive each other, and both have to survive being
// carried into the edit form — otherwise every click costs the reader their place.
func TestFeedsPageViewSurvivesEveryLink(t *testing.T) {
	rd, _, ids := listFixture(t)

	body := rd.body("/feeds?q=comics&sort=unread&dir=desc")

	// The filter form carries the ordering in hidden fields, so filtering again does
	// not silently re-sort the list.
	for _, want := range []string{
		`<input type="hidden" name="sort" value="unread">`,
		`<input type="hidden" name="dir" value="desc">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the filter form does not carry the ordering (%s):\n%s", want, body)
		}
	}

	// A row's edit link carries both, so Cancel and Save come back to this view.
	edit := `href="/feeds?dir=desc&amp;edit=` + strconv.FormatInt(int64(ids["Daily Panel"]), 10) +
		`&amp;q=comics&amp;sort=unread"`
	if !strings.Contains(body, edit) {
		t.Errorf("the row's edit link is not %s:\n%s", edit, body)
	}

	// And the form itself posts back with the view attached.
	opened := rd.body("/feeds?q=comics&sort=unread&dir=desc&edit=" +
		strconv.FormatInt(int64(ids["Daily Panel"]), 10))
	if !strings.Contains(opened, "q=comics") || !strings.Contains(opened, "sort=unread") {
		t.Errorf("the edit form drops the view it was opened from:\n%s", opened)
	}
}

// The form is above the list. With seventy subscriptions in the table, a form
// underneath is a form nobody scrolls to — and this is the assertion that keeps it
// there, because nothing else about the markup depends on the order.
func TestFeedsPagePutsTheFormAboveTheList(t *testing.T) {
	rd, _, _ := listFixture(t)

	body := rd.body("/feeds")

	form := strings.Index(body, `class="add-feed`)
	table := strings.Index(body, `class="feed-table"`)
	imports := strings.Index(body, `class="import"`)

	switch {
	case form < 0 || table < 0 || imports < 0:
		t.Fatalf("the page is missing the form, the table or the import section:\n%s", body)
	case form > table:
		t.Error("the add-a-feed form is below the feed list")
	case imports < table:
		// Bulk import stays at the bottom: it is the rarer errand, and it is the one
		// that does not need the list next to it.
		t.Error("the OPML import moved above the feed list")
	}
}
