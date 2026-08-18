package store_test

import (
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The property worth holding onto is not "next returns something" but "next
// returns what the list showed next". Anything weaker permits a Next button that
// skips articles, which is invisible until a reader notices they never saw
// something they were subscribed to.
func TestNeighborsMatchWhatTheListShowed(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	const total = 7
	list := seedStream(t, s, userID, total) // newest first

	for i, id := range list {
		got, err := s.NeighborsIn(ctx, userID, store.StreamQuery{}, id)
		if err != nil {
			t.Fatalf("NeighborsIn(%d) = %v", id, err)
		}

		var wantNewer, wantOlder store.ArticleID
		if i > 0 {
			wantNewer = list[i-1]
		}
		if i < len(list)-1 {
			wantOlder = list[i+1]
		}

		if got.Newer != wantNewer || got.Older != wantOlder {
			t.Errorf("neighbors of list position %d (article %d) = newer %d, older %d; want %d, %d",
				i, id, got.Newer, got.Older, wantNewer, wantOlder)
		}
	}
}

// The ends of a list are ends, not wraps. Wrapping from the last article back to
// the first is how a reader loses track of whether they have finished.
func TestNeighborsDoNotWrap(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	list := seedStream(t, s, userID, 3)

	newest, err := s.NeighborsIn(ctx, userID, store.StreamQuery{}, list[0])
	if err != nil {
		t.Fatalf("NeighborsIn(newest) = %v", err)
	}
	if newest.Newer != 0 {
		t.Errorf("the newest article has a newer neighbor %d, want none", newest.Newer)
	}

	oldest, err := s.NeighborsIn(ctx, userID, store.StreamQuery{}, list[len(list)-1])
	if err != nil {
		t.Fatalf("NeighborsIn(oldest) = %v", err)
	}
	if oldest.Older != 0 {
		t.Errorf("the oldest article has an older neighbor %d, want none", oldest.Older)
	}
}

// Neighbors respect the list's filter. "Next" in Starred means the next starred
// article, not the next article.
func TestNeighborsRespectTheListsFilter(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	list := seedStream(t, s, userID, 5) // newest first

	// Star the first, third and fifth, so the starred list skips two.
	for _, i := range []int{0, 2, 4} {
		if _, err := s.SetStarred(ctx, userID, list[i], true); err != nil {
			t.Fatalf("SetStarred() = %v", err)
		}
	}

	got, err := s.NeighborsIn(ctx, userID, store.StreamQuery{StarredOnly: true}, list[2])
	if err != nil {
		t.Fatalf("NeighborsIn() = %v", err)
	}
	if got.Newer != list[0] || got.Older != list[4] {
		t.Errorf("starred neighbors of list[2] = %d / %d, want %d / %d (skipping the unstarred)",
			got.Newer, got.Older, list[0], list[4])
	}
}

// Opening an article marks it read, so a strictly-unread list would drop it the
// instant a reader arrived — and then "previous" would point off the top of a list
// holding nothing they had seen. ReadWithin is what keeps the list still for the
// length of a reading session.
func TestNeighborsInUnreadSurviveTheArticleBeingRead(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	list := seedStream(t, s, userID, 5) // newest first
	middle := list[2]

	// The reader opens it, which is what marks it read.
	if _, err := s.SetRead(ctx, userID, middle, true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}

	unread := store.StreamQuery{UnreadOnly: true}

	// Without the window, the article is no longer in the list its neighbors are
	// computed from. Its neighbors are still well defined — the position lookup
	// ignores the filter — but they close over the gap it left.
	tight, err := s.NeighborsIn(ctx, userID, unread, middle)
	if err != nil {
		t.Fatalf("NeighborsIn(no window) = %v", err)
	}
	if tight.Newer != list[1] || tight.Older != list[3] {
		t.Errorf("neighbors without a window = %d / %d, want %d / %d",
			tight.Newer, tight.Older, list[1], list[3])
	}

	// Now read the one above it too, as a reader working down the list would. With
	// the window, going back still reaches it.
	if _, err := s.SetRead(ctx, userID, list[1], true); err != nil {
		t.Fatalf("SetRead() = %v", err)
	}

	unread.ReadWithin = 30 * time.Minute
	windowed, err := s.NeighborsIn(ctx, userID, unread, middle)
	if err != nil {
		t.Fatalf("NeighborsIn(window) = %v", err)
	}
	if windowed.Newer != list[1] {
		t.Errorf("with a window, the previous article = %d, want the just-read %d",
			windowed.Newer, list[1])
	}

	// And without it, the just-read article above is gone from the list, so
	// "previous" skips past it to the one still unread.
	unread.ReadWithin = 0
	skipped, err := s.NeighborsIn(ctx, userID, unread, middle)
	if err != nil {
		t.Fatalf("NeighborsIn(no window, again) = %v", err)
	}
	if skipped.Newer != list[0] {
		t.Errorf("without a window, the previous article = %d, want %d (skipping the just-read one)",
			skipped.Newer, list[0])
	}
}

// An article the reader cannot see has no neighbors, and asking is not an error.
// Reporting one would tell them the difference between "not yours" and "not there".
func TestNeighborsOfAnInvisibleArticleAreEmpty(t *testing.T) {
	_, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	seedStream(t, s, userID, 3)

	got, err := s.NeighborsIn(ctx, userID, store.StreamQuery{}, store.ArticleID(999999))
	if err != nil {
		t.Fatalf("NeighborsIn(missing) = %v, want no error", err)
	}
	if got.Newer != 0 || got.Older != 0 {
		t.Errorf("NeighborsIn(missing) = %+v, want zeroes", got)
	}
}
