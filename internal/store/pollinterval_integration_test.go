package store_test

import (
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// A reader's own polling cadence: the general preference, the per-feed override,
// and what each of them does to a schedule that was already set.
//
// The scheduling half is what these are really for. Storing an interval is one
// UPDATE; the part that goes wrong is a shortened cadence that does not take effect
// until the poll it was meant to replace, which presents as a setting that does
// nothing.

// editing is the feed's current state as an edit, so a test can change one field
// without restating the row.
func editing(t *testing.T, s *store.Store, userID store.UserID, id store.FeedID) store.FeedEdit {
	t.Helper()

	f, err := s.GetFeed(t.Context(), userID, id)
	if err != nil {
		t.Fatalf("GetFeed(%d) = %v", id, err)
	}
	return store.FeedEdit{
		FeedURL:      f.FeedURL,
		Title:        f.Title,
		Category:     f.Category,
		Disabled:     f.Disabled,
		PollInterval: f.PollIntervalOverride,
	}
}

// untilNextPoll is how long a feed has left before it is due. Negative means it is
// due now.
func untilNextPoll(t *testing.T, s *store.Store, userID store.UserID, id store.FeedID) time.Duration {
	t.Helper()

	f, err := s.GetFeed(t.Context(), userID, id)
	if err != nil {
		t.Fatalf("GetFeed(%d) = %v", id, err)
	}
	return time.Until(f.NextPollAt)
}

func hours(n int) *time.Duration {
	d := time.Duration(n) * time.Hour
	return &d
}

// The resolution order, read back through the same query the poller uses. The
// general preference has to arrive on the feed row: the poller resolves this per
// poll and must not need a second query per feed to do it.
func TestFeedCarriesBothCadencesAndPrefersItsOwn(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	long := 4 * time.Hour
	id := subscribe(t, pool, s, userID, "quarterly", &long, false)

	// Neither set: the poller decides.
	before, err := s.GetFeed(ctx, userID, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if _, ok := before.ChosenInterval(); ok {
		t.Error("a feed nobody has set a cadence on reports one")
	}

	if _, err := s.SetDefaultPollInterval(ctx, userID, hours(3)); err != nil {
		t.Fatalf("SetDefaultPollInterval() = %v", err)
	}

	withDefault, err := s.GetFeed(ctx, userID, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if withDefault.PollIntervalOverride != nil {
		t.Error("the general preference was written onto the feed as an override")
	}
	if got, ok := withDefault.ChosenInterval(); !ok || got != 3*time.Hour {
		t.Errorf("ChosenInterval() = %v, %v; want the reader's 3h", got, ok)
	}

	// The one feed the preference is wrong for.
	edit := editing(t, s, userID, id)
	edit.PollInterval = hours(24)
	updated, err := s.UpdateFeed(ctx, userID, id, edit)
	if err != nil {
		t.Fatalf("UpdateFeed() = %v", err)
	}
	// Read from the UPDATE's own RETURNING, which is the awkward place for the
	// preference to be reachable from and therefore the one worth asserting on.
	if updated.PollIntervalOverride == nil || *updated.PollIntervalOverride != 24*time.Hour {
		t.Errorf("override after the edit = %v, want 24h", updated.PollIntervalOverride)
	}
	if updated.DefaultPollInterval == nil || *updated.DefaultPollInterval != 3*time.Hour {
		t.Errorf("the reader's preference did not survive RETURNING: %v", updated.DefaultPollInterval)
	}
	if got, ok := updated.ChosenInterval(); !ok || got != 24*time.Hour {
		t.Errorf("ChosenInterval() = %v, %v; want the feed's own 24h", got, ok)
	}

	// And back to automatic, which is a value like any other and must be storable.
	edit.PollInterval = nil
	cleared, err := s.UpdateFeed(ctx, userID, id, edit)
	if err != nil {
		t.Fatalf("UpdateFeed() = %v", err)
	}
	if cleared.PollIntervalOverride != nil {
		t.Errorf("clearing the override left %v", *cleared.PollIntervalOverride)
	}
	if got, ok := cleared.ChosenInterval(); !ok || got != 3*time.Hour {
		t.Errorf("ChosenInterval() = %v, %v after clearing; want the preference back", got, ok)
	}
}

// A shortened cadence has to move the schedule, or it does not take effect until
// the poll it was meant to change — up to a day away.
func TestShorteningAFeedsCadenceBringsItsPollForward(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	// Polled four hours ago, next poll parked twelve hours out.
	long := 4 * time.Hour
	id := subscribe(t, pool, s, userID, "parked", &long, false)

	edit := editing(t, s, userID, id)
	edit.PollInterval = hours(1)
	if _, err := s.UpdateFeed(ctx, userID, id, edit); err != nil {
		t.Fatalf("UpdateFeed() = %v", err)
	}

	// Hourly, and it was last polled four hours ago, so it is due — but by the
	// cadence, not by a refresh: the bound is last poll + interval.
	if left := untilNextPoll(t, s, userID, id); left > 0 {
		t.Errorf("next poll is %v away, want it due now for a feed polled 4h ago on an hourly cadence", left)
	}
}

// The other direction, which is the one that could quietly cost a reader their
// articles: choosing a longer cadence must not push a feed that is nearly due out
// to the end of the new interval.
func TestLengtheningACadenceLeavesAnImminentPollAlone(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	recent := time.Minute
	id := subscribe(t, pool, s, userID, "soon", &recent, false)

	// Bring it forward to a few minutes from now, the way a manual refresh or a
	// short learned interval would.
	if _, err := pool.Exec(ctx,
		`UPDATE feeds SET next_poll_at = now() + interval '5 minutes' WHERE id = $1`, id); err != nil {
		t.Fatalf("setting next_poll_at: %v", err)
	}

	edit := editing(t, s, userID, id)
	edit.PollInterval = hours(24)
	if _, err := s.UpdateFeed(ctx, userID, id, edit); err != nil {
		t.Fatalf("UpdateFeed() = %v", err)
	}

	if left := untilNextPoll(t, s, userID, id); left > 10*time.Minute {
		t.Errorf("next poll moved out to %v; a longer cadence must not postpone a poll "+
			"that was already imminent", left)
	}
}

// An edit that is about a title must not move the schedule, and neither must
// choosing automatic — there is no cadence to bring anything forward to.
func TestAnEditWithNoCadenceChangeLeavesTheScheduleAlone(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	long := 4 * time.Hour
	id := subscribe(t, pool, s, userID, "renamed", &long, false)

	before := untilNextPoll(t, s, userID, id)

	edit := editing(t, s, userID, id)
	edit.Title = "A Better Name"
	if _, err := s.UpdateFeed(ctx, userID, id, edit); err != nil {
		t.Fatalf("UpdateFeed() = %v", err)
	}

	if after := untilNextPoll(t, s, userID, id); after < before-time.Minute {
		t.Errorf("next poll moved from %v to %v on an edit that only changed the title",
			before, after)
	}
}

// The general preference, and the three kinds of feed it must not touch.
func TestSettingTheGeneralCadenceMovesOnlyWhatItOwns(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	long := 4 * time.Hour
	plain := subscribe(t, pool, s, userID, "plain", &long, false)
	overridden := subscribe(t, pool, s, userID, "overridden", &long, false)
	dead := subscribe(t, pool, s, userID, "dead", &long, true)

	edit := editing(t, s, userID, overridden)
	edit.PollInterval = hours(24)
	if _, err := s.UpdateFeed(ctx, userID, overridden, edit); err != nil {
		t.Fatalf("UpdateFeed() = %v", err)
	}

	moved, err := s.SetDefaultPollInterval(ctx, userID, hours(1))
	if err != nil {
		t.Fatalf("SetDefaultPollInterval() = %v", err)
	}
	if moved != 1 {
		t.Errorf("SetDefaultPollInterval brought %d feeds forward, want only the one with "+
			"no cadence of its own", moved)
	}

	if left := untilNextPoll(t, s, userID, plain); left > 0 {
		t.Errorf("a feed on the general cadence is %v from due, want due now", left)
	}
	// Its own cadence is 24h and it was polled 4h ago, so it stays where it was.
	if left := untilNextPoll(t, s, userID, overridden); left <= 0 {
		t.Error("a feed with its own cadence was brought forward by the general preference")
	}
	if left := untilNextPoll(t, s, userID, dead); left <= 0 {
		t.Error("a disabled feed was brought forward; a cadence is not the place to revive one")
	}
}

// Going back to automatic stores the absence and changes no schedule: there is no
// cadence left to measure a poll against.
func TestReturningToAutomaticKeepsTheSchedule(t *testing.T) {
	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()

	long := 4 * time.Hour
	id := subscribe(t, pool, s, userID, "was-hourly", &long, false)

	if _, err := s.SetDefaultPollInterval(ctx, userID, hours(1)); err != nil {
		t.Fatalf("SetDefaultPollInterval() = %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE feeds SET next_poll_at = now() + interval '6 hours' WHERE id = $1`, id); err != nil {
		t.Fatalf("setting next_poll_at: %v", err)
	}

	moved, err := s.SetDefaultPollInterval(ctx, userID, nil)
	if err != nil {
		t.Fatalf("SetDefaultPollInterval(nil) = %v", err)
	}
	if moved != 0 {
		t.Errorf("returning to automatic moved %d feeds, want 0", moved)
	}

	prefs, err := s.GetPreferences(ctx, userID)
	if err != nil {
		t.Fatalf("GetPreferences() = %v", err)
	}
	if prefs.DefaultPollInterval != nil {
		t.Errorf("preference after returning to automatic = %v, want nil", *prefs.DefaultPollInterval)
	}
	if left := untilNextPoll(t, s, userID, id); left < 5*time.Hour {
		t.Errorf("next poll moved to %v away; automatic has no cadence to reschedule by", left)
	}
}

// One reader's cadence is not another's, including through the subquery that puts
// the preference on every feed row.
func TestCadenceIsScopedToOneReader(t *testing.T) {
	pool, s, alice := dbtest.SetupWithUser(t)
	ctx := t.Context()

	var bob store.UserID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("creating a second reader: %v", err)
	}

	long := 4 * time.Hour
	hers := subscribe(t, pool, s, alice, "hers", &long, false)
	his := subscribe(t, pool, s, bob, "his", &long, false)

	if _, err := s.SetDefaultPollInterval(ctx, alice, hours(1)); err != nil {
		t.Fatalf("SetDefaultPollInterval() = %v", err)
	}

	hersFeed, err := s.GetFeed(ctx, alice, hers)
	if err != nil {
		t.Fatalf("GetFeed(alice) = %v", err)
	}
	if got, ok := hersFeed.ChosenInterval(); !ok || got != time.Hour {
		t.Errorf("her feed's cadence = %v, %v; want her 1h", got, ok)
	}

	hisFeed, err := s.GetFeed(ctx, bob, his)
	if err != nil {
		t.Fatalf("GetFeed(bob) = %v", err)
	}
	if hisFeed.DefaultPollInterval != nil {
		t.Errorf("her preference reached his feed as %v", *hisFeed.DefaultPollInterval)
	}
	if left := untilNextPoll(t, s, bob, his); left <= 0 {
		t.Error("her preference brought his feed forward")
	}
}
