package server

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// Ordering and filtering the feed list, and the links that carry that choice
// through everything else on the page.
//
// Both happen in Go over the rows the page has already loaded, which is worth
// being explicit about because the database looks like the right place for it. The
// page needs every row regardless — the banner counts how many feeds are failing,
// the category suggestions come out of the same list — and two of the sortable
// columns are not columns at all: the unread count arrives from a separate
// aggregate query, and health is a rank over three fields. Sorting in SQL would
// mean an ORDER BY variant per key plus a join for the one key it still could not
// order by, in exchange for nothing: the filter costs a substring test per row
// against data already in memory, so there is no query behind it to pay for.
//
// The state lives in the query string rather than in a cookie or a session,
// because a sorted, filtered list is a thing people bookmark and send to
// themselves, and because a page that renders from its own URL is a page that
// cannot get out of step with what the reader is looking at.

// feedSort is the column the list is ordered by.
type feedSort string

const (
	// sortStored is the order the store returns: category, then title. It is the
	// default, and it is not reachable from a column heading — the headings sort by
	// one column each, and this is the grouping the list has when nobody has asked
	// for anything.
	sortStored   feedSort = ""
	sortTitle    feedSort = "title"
	sortCategory feedSort = "category"
	sortUnread   feedSort = "unread"
	sortLast     feedSort = "last"
	sortHealth   feedSort = "health"
)

// feedHealth is the health filter.
type feedHealth string

const (
	healthAny      feedHealth = ""
	healthOK       feedHealth = "ok"
	healthFailing  feedHealth = "failing"
	healthDisabled feedHealth = "disabled"
)

// feedView is what the reader has asked the list to show.
type feedView struct {
	Sort feedSort
	Desc bool

	// Query is matched against a feed's title, address and category. One box rather
	// than a control per column: "which of these is the one I mean" is how somebody
	// actually searches a list of seventy subscriptions, and a per-column form would
	// be four controls to serve the same question.
	Query string

	Health feedHealth
}

// feedViewFrom reads the view out of a request's query string.
//
// Anything unrecognized falls back to the default rather than failing. These
// parameters are typed by hand and pasted between tabs, and a mistyped sort key
// should show the list, not an error page about a list that is right there.
func feedViewFrom(q url.Values) feedView {
	v := feedView{Query: strings.TrimSpace(q.Get("q"))}

	switch key := feedSort(q.Get("sort")); key {
	case sortTitle, sortCategory, sortUnread, sortLast, sortHealth:
		v.Sort = key
		// A direction is only meaningful next to a key, so it is read only here.
		// Otherwise `?dir=desc` alone would claim to reverse an order it has no
		// say over.
		switch q.Get("dir") {
		case "desc":
			v.Desc = true
		case "asc":
			v.Desc = false
		default:
			v.Desc = key.firstDescending()
		}
	}

	switch health := feedHealth(q.Get("health")); health {
	case healthOK, healthFailing, healthDisabled:
		v.Health = health
	}

	return v
}

// firstDescending is which way a column sorts the first time it is clicked.
//
// Not uniformly ascending, because the useful end differs by column: an alphabet
// reads from A, but nobody clicks Unread hoping to be shown the feeds with nothing
// in them, and nobody clicks Health to see the healthy ones first. Last success is
// ascending on purpose — oldest and never first — for the same reason: the point of
// sorting by it is to find what has gone quiet.
func (s feedSort) firstDescending() bool { return s == sortUnread || s == sortHealth }

// Filtered reports whether anything is being hidden, which is what makes the count
// line and the clear control worth drawing.
func (v feedView) Filtered() bool { return v.Query != "" || v.Health != healthAny }

// HealthIs backs the filter's select, because html/template's eq will not compare
// a feedHealth against a string literal.
func (v feedView) HealthIs(health string) bool { return v.Health == feedHealth(health) }

// SortKey is the current key as a plain string, for the hidden field that carries
// the ordering through the filter form.
func (v feedView) SortKey() string { return string(v.Sort) }

// Direction is the current direction, as the query string spells it. Empty when no
// column is sorted, so the form omits the field entirely.
func (v feedView) Direction() string {
	if v.Sort == sortStored {
		return ""
	}
	if v.Desc {
		return "desc"
	}
	return "asc"
}

// values renders the view as query parameters, leaving out anything at its default
// so that an unsorted, unfiltered list is plain /feeds and not a URL full of
// empties.
func (v feedView) values() url.Values {
	q := url.Values{}
	if v.Query != "" {
		q.Set("q", v.Query)
	}
	if v.Health != healthAny {
		q.Set("health", string(v.Health))
	}
	if v.Sort != sortStored {
		q.Set("sort", string(v.Sort))
		q.Set("dir", v.Direction())
	}
	return q
}

// href is a path with this view's parameters on it, plus any extra.
func (v feedView) href(path string, extra ...[2]string) string {
	q := v.values()
	for _, kv := range extra {
		q.Set(kv[0], kv[1])
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// SortHref is the link on a column heading: sort by this column, or reverse it if
// the list is already sorted by it.
func (v feedView) SortHref(key string) string {
	next := feedView{Query: v.Query, Health: v.Health, Sort: feedSort(key)}
	if v.Sort == next.Sort {
		next.Desc = !v.Desc
	} else {
		next.Desc = next.Sort.firstDescending()
	}
	return next.href("/feeds")
}

// AriaSort is the heading's aria-sort value, empty for a column that is not the
// one being sorted by — the attribute is meaningful only on that column, and
// aria-sort="none" on the other four says the same thing at four times the noise.
func (v feedView) AriaSort(key string) string {
	if v.Sort != feedSort(key) {
		return ""
	}
	if v.Desc {
		return "descending"
	}
	return "ascending"
}

// EditHref opens one feed in the form at the top of the page, and keeps the view
// so that saving or canceling comes back to the list the reader was looking at
// rather than to the top of an unfiltered one.
func (v feedView) EditHref(id store.FeedID) string {
	return v.href("/feeds", [2]string{"edit", strconv.FormatInt(int64(id), 10)})
}

// UnsubscribeHref asks about removing one subscription, keeping the view so that
// answering either way comes back to the list the reader was looking at.
func (v feedView) UnsubscribeHref(id store.FeedID) string {
	return v.href("/feeds", [2]string{"unsubscribe", strconv.FormatInt(int64(id), 10)})
}

// ClearHref drops the filter and keeps the ordering. Two separate ideas: somebody
// clearing a search has not asked to be re-sorted.
func (v feedView) ClearHref() string {
	return feedView{Sort: v.Sort, Desc: v.Desc}.href("/feeds")
}

// CancelHref closes the form without saving.
func (v feedView) CancelHref() string { return v.href("/feeds") }

// matches reports whether a row survives the filter.
func (v feedView) matches(row feedRow) bool {
	switch v.Health {
	case healthDisabled:
		if !row.Disabled {
			return false
		}
	case healthFailing:
		// Disabled feeds are deliberately not "failing" here. They have stopped
		// being polled, so they are a state of their own rather than the worst end
		// of this one — and somebody asking which feeds are failing is asking what
		// is going wrong now.
		if row.Disabled || row.ConsecutiveFailures == 0 {
			return false
		}
	case healthOK:
		if row.Disabled || row.ConsecutiveFailures > 0 {
			return false
		}
	}

	if v.Query == "" {
		return true
	}
	needle := strings.ToLower(v.Query)
	return strings.Contains(strings.ToLower(row.Title), needle) ||
		strings.Contains(strings.ToLower(row.FeedURL), needle) ||
		strings.Contains(strings.ToLower(row.Category), needle)
}

// sortRows orders rows in place.
//
// Stable, and the direction is applied by swapping the comparison's arguments
// rather than by reversing the slice afterwards. That is the difference between
// rows a key cannot separate keeping the store's order — category, then title —
// and having that order reversed along with everything else, which is how a table
// ends up looking shuffled every time somebody clicks a heading twice.
func (v feedView) sortRows(rows []feedRow) {
	less := lessBy(v.Sort)
	if less == nil {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if v.Desc {
			return less(rows[j], rows[i])
		}
		return less(rows[i], rows[j])
	})
}

// lessBy is the ascending comparison for one key, or nil for the stored order,
// which is already what the rows arrived in.
func lessBy(key feedSort) func(a, b feedRow) bool {
	switch key {
	case sortTitle:
		return func(a, b feedRow) bool { return foldLess(a.displayName(), b.displayName()) }
	case sortCategory:
		// Uncategorized last in ascending order, which matches the category index:
		// a folder name is a decision the reader made, and the leftovers belong at
		// the end of the list rather than the front of it.
		return func(a, b feedRow) bool {
			if (a.Category == "") != (b.Category == "") {
				return b.Category == ""
			}
			return foldLess(a.Category, b.Category)
		}
	case sortUnread:
		return func(a, b feedRow) bool { return a.Unread < b.Unread }
	case sortLast:
		// A feed that has never succeeded sorts as the oldest thing there is, which
		// is what it is: nothing has ever arrived from it.
		return func(a, b feedRow) bool { return lastSuccess(a).Before(lastSuccess(b)) }
	case sortHealth:
		return func(a, b feedRow) bool {
			if a.healthRank() != b.healthRank() {
				return a.healthRank() < b.healthRank()
			}
			return a.ConsecutiveFailures < b.ConsecutiveFailures
		}
	default:
		return nil
	}
}

// foldLess compares case-insensitively, so that "BBC" and "bbc" sort as neighbors
// rather than in two alphabets — feed titles come from whatever the publisher
// wrote, and half of them are capitalized differently from the rest.
func foldLess(a, b string) bool {
	folded, other := strings.ToLower(a), strings.ToLower(b)
	if folded != other {
		return folded < other
	}
	return a < b
}

func lastSuccess(r feedRow) time.Time {
	if r.LastSuccessAt == nil {
		return time.Time{}
	}
	return *r.LastSuccessAt
}

// displayName is what the row shows in its first column, which is what sorting by
// that column has to agree with: a feed with no title of its own is listed by its
// address, so ordering it by an empty string would file it under nothing.
func (r feedRow) displayName() string {
	if r.Title != "" {
		return r.Title
	}
	return r.FeedURL
}

// healthRank orders the health column, worst last in ascending order.
func (r feedRow) healthRank() int {
	switch {
	case r.Disabled:
		return 2
	case r.ConsecutiveFailures > 0:
		return 1
	default:
		return 0
	}
}
