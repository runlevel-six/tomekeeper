package feed_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// What a reader's chosen cadence wins against, and the two things it does not.
//
// Every case here is one line of the precedence order in Poller.nextInterval, and
// they are worth testing at this level rather than as a unit on the policy: the
// order is the feature, and the policy cannot see it — it is handed one interval at
// a time and told what to do with it.

// declaresHourlyTwice is a feed that says it changes twice an hour, which the
// policy turns into 30 minutes.
const declaresHourlyTwice = `<?xml version="1.0"?>
<rss version="2.0" xmlns:sy="http://purl.org/rss/1.0/modules/syndication/">
  <channel>
    <title>Declared</title>
    <sy:updatePeriod>hourly</sy:updatePeriod>
    <sy:updateFrequency>2</sy:updateFrequency>
    <item><title>Post</title><link>https://example.com/p</link><guid>p</guid></item>
  </channel>
</rss>`

// serving returns a test server that answers every request with body.
func serving(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hours(n int) *time.Duration {
	d := time.Duration(n) * time.Hour
	return &d
}

// The case the feature exists for: an explicit choice beats the publisher's own
// declaration. Arguable — the publisher does know their schedule — and settled the
// other way on the grounds that a setting whose effect depends on invisible markup
// is not a setting.
func TestChosenCadenceBeatsTheFeedsOwnDeclaration(t *testing.T) {
	srv := serving(t, declaresHourlyTwice)

	f := testFeed(srv.URL)
	f.PollIntervalOverride = hours(6)

	fake := newFakeStore(f)
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	if got, want := res.NextInterval, 6*time.Hour; got != want {
		t.Errorf("NextInterval = %v, want the chosen %v rather than the declared 30m", got, want)
	}
}

// The general preference, on a feed with no opinion of its own.
func TestReadersGeneralCadenceAppliesWithoutAnOverride(t *testing.T) {
	srv := serving(t, rssTwoItems)

	f := testFeed(srv.URL)
	f.DefaultPollInterval = hours(3)

	fake := newFakeStore(f)
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	// Without the preference this poll found new items and would have halved the
	// stored hour to 30 minutes.
	if got, want := res.NextInterval, 3*time.Hour; got != want {
		t.Errorf("NextInterval = %v, want the reader's %v", got, want)
	}
}

// The whole point of having both settings: the preference is what to do with
// seventy feeds, and the override is the one feed it is wrong for.
func TestFeedOverrideBeatsTheGeneralCadence(t *testing.T) {
	srv := serving(t, rssTwoItems)

	f := testFeed(srv.URL)
	f.DefaultPollInterval = hours(12)
	f.PollIntervalOverride = hours(1)

	fake := newFakeStore(f)
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	if got, want := res.NextInterval, time.Hour; got != want {
		t.Errorf("NextInterval = %v, want the feed's own %v", got, want)
	}
}

// The floor belongs to the operator, not the reader: TOME_POLL_MIN_INTERVAL is a
// promise made to other people's servers, and a dropdown cannot spend their request
// budget.
func TestAChosenCadenceIsRaisedToTheFloor(t *testing.T) {
	srv := serving(t, rssTwoItems)

	f := testFeed(srv.URL)
	tooOften := time.Minute
	f.PollIntervalOverride = &tooOften

	fake := newFakeStore(f)
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	if got, want := res.NextInterval, 15*time.Minute; got != want {
		t.Errorf("NextInterval = %v, want the %v floor", got, want)
	}
}

// A 304 has no body, so it is the one path that cannot consult the feed's own
// declaration — it must still honor the reader's.
func TestChosenCadenceSurvivesANotModifiedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	f := testFeed(srv.URL)
	f.ETag = `"unchanged"`
	f.PollIntervalOverride = hours(6)

	fake := newFakeStore(f)
	if _, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID); err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	if len(fake.notModified) != 1 {
		t.Fatalf("recorded %d 304s, want 1", len(fake.notModified))
	}
	// The adaptive answer here would be an hour and a half — the stored hour grown
	// by 1.5 for finding nothing.
	if got, want := fake.notModified[0], 6*time.Hour; got != want {
		t.Errorf("interval after 304 = %v, want the chosen %v", got, want)
	}
}

// Backoff starts from the floor, so without the chosen cadence acting as a floor on
// it a feed set to weekly would be polled every 15 minutes the moment it broke —
// hundreds of times more often than the reader asked for it while it worked.
func TestFailureBackoffIsNeverShorterThanTheChosenCadence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := testFeed(srv.URL)
	f.PollIntervalOverride = hours(6)

	fake := newFakeStore(f)
	if _, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID); err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	if len(fake.failures) != 1 {
		t.Fatalf("recorded %d failures, want 1", len(fake.failures))
	}
	if got, want := fake.failures[0].Interval, 6*time.Hour; got != want {
		t.Errorf("interval after a failure = %v, want the chosen %v rather than the 15m the "+
			"backoff starts from", got, want)
	}
}

// The other half of that: a cadence is a floor on the backoff and not a ceiling.
// A feed that has failed six times running is not a feed whose publishing schedule
// is the question.
func TestFailureBackoffStillGrowsPastTheChosenCadence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := testFeed(srv.URL)
	f.PollIntervalOverride = hours(1)
	f.ConsecutiveFailures = 5 // the sixth failure: 15m × 2⁵

	fake := newFakeStore(f)
	if _, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID); err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	if len(fake.failures) != 1 {
		t.Fatalf("recorded %d failures, want 1", len(fake.failures))
	}
	if got, want := fake.failures[0].Interval, 8*time.Hour; got != want {
		t.Errorf("interval after six failures = %v, want the backoff's %v", got, want)
	}
}

// An interval the picker never offered, because a reader who set one by hand is
// still a reader who chose it.
func TestAnOffListCadenceIsHonoredAsGiven(t *testing.T) {
	srv := serving(t, rssTwoItems)

	f := testFeed(srv.URL)
	odd := 47 * time.Minute
	f.PollIntervalOverride = &odd

	fake := newFakeStore(f)
	res, err := newPoller(t, fake).Poll(t.Context(), testUserID, testFeedID)
	if err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	if got := res.NextInterval; got != odd {
		t.Errorf("NextInterval = %v, want %v", got, odd)
	}
}

// The store's own resolution order, without a poll around it.
func TestChosenIntervalReportsWhetherAnythingWasChosen(t *testing.T) {
	var f store.Feed
	if _, ok := f.ChosenInterval(); ok {
		t.Error("ChosenInterval() reported a choice on a feed with neither setting")
	}

	f.DefaultPollInterval = hours(3)
	if got, ok := f.ChosenInterval(); !ok || got != 3*time.Hour {
		t.Errorf("ChosenInterval() = %v, %v with only a general preference; want 3h, true", got, ok)
	}

	f.PollIntervalOverride = hours(1)
	if got, ok := f.ChosenInterval(); !ok || got != time.Hour {
		t.Errorf("ChosenInterval() = %v, %v with both set; want the override 1h, true", got, ok)
	}
}
