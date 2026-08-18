package store_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// subscribe adds a feed and puts its polling state where the test needs it.
//
// The columns are set directly because there is no legitimate path to "last
// polled four hours ago" other than waiting four hours.
func subscribe(t *testing.T, pool *pgxpool.Pool, s *store.Store, userID store.UserID,
	slug string, lastPolled *time.Duration, disabled bool,
) store.FeedID {
	t.Helper()
	ctx := t.Context()

	id, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://example.com/" + slug + "/feed.xml", Title: slug,
	})
	if err != nil {
		t.Fatalf("UpsertFeed(%s) = %v", slug, err)
	}

	var ago any
	if lastPolled != nil {
		ago = lastPolled.Seconds()
	}
	if _, err := pool.Exec(ctx, `
		UPDATE feeds SET
			disabled       = $2,
			last_polled_at = CASE WHEN $3::float8 IS NULL
			                      THEN NULL
			                      ELSE now() - make_interval(secs => $3) END,
			-- Well into the future, so a poll being brought forward is a visible
			-- change rather than something that was going to happen anyway.
			next_poll_at   = now() + interval '12 hours'
		WHERE id = $1`, id, disabled, ago); err != nil {
		t.Fatalf("setting up feed %s: %v", slug, err)
	}
	return id
}

func TestPollNowBringsFeedsForwardAndCountsHonestly(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	long := 4 * time.Hour
	recent := time.Minute

	stale := subscribe(t, pool, s, userID, "stale", &long, false)
	never := subscribe(t, pool, s, userID, "never", nil, false)
	fresh := subscribe(t, pool, s, userID, "fresh", &recent, false)
	dead := subscribe(t, pool, s, userID, "dead", &long, true)

	got, err := s.PollNow(ctx, userID)
	if err != nil {
		t.Fatalf("PollNow() = %v", err)
	}

	want := store.PollNowResult{Moved: 2, Held: 1, Disabled: 1}
	if got != want {
		t.Errorf("PollNow() = %+v, want %+v", got, want)
	}

	// The three numbers must account for every subscription, or the page built
	// from them tells the reader about a different archive than the one they have.
	if total := got.Moved + got.Held + got.Disabled; total != 4 {
		t.Errorf("the counts add up to %d, want one per subscription (4)", total)
	}

	// And the effect, not just the report.
	due := func(id store.FeedID) bool {
		t.Helper()
		f, err := s.GetFeed(ctx, userID, id)
		if err != nil {
			t.Fatalf("GetFeed() = %v", err)
		}
		return !f.NextPollAt.After(time.Now())
	}
	for _, c := range []struct {
		name string
		id   store.FeedID
		want bool
	}{
		{"a feed polled hours ago", stale, true},
		{"a feed never polled", never, true},
		{"a feed polled a minute ago", fresh, false},
		// Not revived. A refresh that quietly re-enabled everything auto-disable
		// had turned off would undo the feature on every reload.
		{"a disabled feed", dead, false},
	} {
		if due(c.id) != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, due(c.id), c.want)
		}
	}
}

// Pressing the button twice must not cost the origins a second round of requests.
// That is what the floor is for, and it is the one property a reader can trigger
// by accident.
func TestPollNowIsCheapToRepeat(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	long := 4 * time.Hour
	feedID := subscribe(t, pool, s, userID, "stale", &long, false)

	first, err := s.PollNow(ctx, userID)
	if err != nil {
		t.Fatalf("PollNow() = %v", err)
	}
	if first.Moved != 1 {
		t.Fatalf("the first refresh moved %d feeds, want 1", first.Moved)
	}

	// Pretend the worker got there: that is what puts the feed inside the floor.
	if err := s.RecordPollNotModified(ctx, userID, feedID, time.Hour); err != nil {
		t.Fatalf("RecordPollNotModified() = %v", err)
	}

	second, err := s.PollNow(ctx, userID)
	if err != nil {
		t.Fatalf("PollNow() = %v", err)
	}
	if second.Moved != 0 || second.Held != 1 {
		t.Errorf("the second refresh = %+v, want nothing moved and one held", second)
	}
}

// One reader's button does not poll another reader's feeds.
func TestPollNowIsScopedToOneReader(t *testing.T) {
	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	var bob store.UserID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("creating the second user: %v", err)
	}

	long := 4 * time.Hour
	subscribe(t, pool, s, alice, "hers", &long, false)
	his := subscribe(t, pool, s, bob, "his", &long, false)

	got, err := s.PollNow(ctx, alice)
	if err != nil {
		t.Fatalf("PollNow() = %v", err)
	}
	if got.Moved != 1 {
		t.Errorf("PollNow(alice) moved %d feeds, want only her 1", got.Moved)
	}

	f, err := s.GetFeed(ctx, bob, his)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if !f.NextPollAt.After(time.Now()) {
		t.Error("Alice's refresh brought Bob's feed forward")
	}
}
