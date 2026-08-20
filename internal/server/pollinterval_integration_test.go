package server_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The two cadence pickers: one on Settings for every feed, one on the feed form for
// a single subscription. What is being tested here is mostly what each control
// reports, because a preference nobody can see the state of is a preference nobody
// trusts.

// editedWith posts the edit form for a feed, changing only the cadence.
func editedWith(t *testing.T, rd *reader, id store.FeedID, feedURL, pollEvery string) *httptest.ResponseRecorder {
	t.Helper()

	return rd.do(http.MethodPost, editPath(id), url.Values{
		"url":        {feedURL},
		"title":      {"Moved Publication"},
		"category":   {"News"},
		"enabled":    {"true"},
		"poll_every": {pollEvery},
	})
}

func TestTheEditFormOffersACadenceAndTheAddFormDoesNot(t *testing.T) {
	rd, tr := readingFixture(t)
	id := brokenFeed(t, tr, "https://news.example.com/old.xml")

	form := rd.body("/feeds?edit=" + strconv.FormatInt(int64(id), 10))
	if !strings.Contains(form, `name="poll_every"`) {
		t.Errorf("the edit form has no cadence picker:\n%s", form)
	}
	// Automatic is what this feed has, and it is what must be selected — an empty
	// select with ten options would read as a choice already made.
	if !strings.Contains(form, `<option value="" selected>Automatically</option>`) {
		t.Errorf("automatic is not the selected option:\n%s", form)
	}
	for _, want := range []string{`value="1h"`, `value="24h"`, `value="168h"`} {
		if !strings.Contains(form, want) {
			t.Errorf("the picker does not offer %s", want)
		}
	}

	// The add form deliberately has none: how often to check a feed nobody has
	// fetched yet is a question with no information behind it.
	if add := rd.body("/feeds"); strings.Contains(add, `name="poll_every"`) {
		t.Errorf("the add form offers a cadence picker:\n%s", add)
	}
}

func TestEditingAFeedStoresItsOwnCadence(t *testing.T) {
	rd, tr := readingFixture(t)
	const feedURL = "https://news.example.com/old.xml"
	id := brokenFeed(t, tr, feedURL)

	res := editedWith(t, rd, id, feedURL, "6h")
	if res.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200", editPath(id), res.Code)
	}

	updated, err := tr.store.GetFeed(t.Context(), tr.alice, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if updated.PollIntervalOverride == nil || *updated.PollIntervalOverride != 6*time.Hour {
		t.Fatalf("stored cadence = %v, want 6h", updated.PollIntervalOverride)
	}

	// The row has no column for it, so a save that changed only the cadence would
	// otherwise look like a save that did nothing.
	form := rd.body("/feeds?edit=" + strconv.FormatInt(int64(id), 10))
	if !strings.Contains(form, `value="6h" selected`) {
		t.Errorf("re-opening the form does not show the stored cadence:\n%s", form)
	}
}

// The picker sets this feed's override, so it must not come up showing the general
// preference: opening a form and saving it would then pin every feed to whatever
// the preference happened to be that day.
func TestTheEditFormDoesNotPresentTheGeneralCadenceAsTheFeedsOwn(t *testing.T) {
	rd, tr := readingFixture(t)
	const feedURL = "https://news.example.com/old.xml"
	id := brokenFeed(t, tr, feedURL)

	if rec := rd.do(http.MethodPost, "/settings", url.Values{
		"palette": {""}, "mode": {""}, "poll_every": {"3h"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("POST /settings = %d\n%s", rec.Code, rec.Body.String())
	}

	form := rd.body("/feeds?edit=" + strconv.FormatInt(int64(id), 10))
	if strings.Contains(form, `value="3h" selected`) {
		t.Errorf("the reader's general cadence is selected as this feed's own:\n%s", form)
	}
	if !strings.Contains(form, `<option value="" selected>Automatically</option>`) {
		t.Errorf("automatic is not selected on a feed with no cadence of its own:\n%s", form)
	}
	// Automatic means something different once a general preference exists, and the
	// form has to say which.
	if !strings.Contains(form, "every 3 hours") {
		t.Errorf("the form does not say what automatic now follows:\n%s", form)
	}

	// And the feed is still on automatic in the database, not pinned to 3h.
	f, err := tr.store.GetFeed(t.Context(), tr.alice, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if f.PollIntervalOverride != nil {
		t.Errorf("the general preference was written onto the feed: %v", *f.PollIntervalOverride)
	}
}

// Stored rather than rounded, because a reader who set 47 minutes by hand chose it.
func TestAnIntervalThePickerDoesNotOfferIsStillShown(t *testing.T) {
	rd, tr := readingFixture(t)
	const feedURL = "https://news.example.com/old.xml"
	id := brokenFeed(t, tr, feedURL)

	if res := editedWith(t, rd, id, feedURL, "47m"); res.Code != http.StatusOK {
		t.Fatalf("POST with an off-list interval = %d, want it accepted", res.Code)
	}

	form := rd.body("/feeds?edit=" + strconv.FormatInt(int64(id), 10))
	if !strings.Contains(form, `value="47m0s" selected`) {
		t.Errorf("the off-list interval is not on the picker:\n%s", form)
	}
}

// A value that means nothing costs the save rather than being read as automatic: a
// cadence that silently reverted to "decide for me" is a preference somebody set,
// was told was saved, and did not get.
func TestACadenceThatMeansNothingChangesNothing(t *testing.T) {
	rd, tr := readingFixture(t)
	const feedURL = "https://news.example.com/old.xml"
	id := brokenFeed(t, tr, feedURL)

	if res := editedWith(t, rd, id, feedURL, "6h"); res.Code != http.StatusOK {
		t.Fatalf("setting up the cadence = %d", res.Code)
	}

	res := editedWith(t, rd, id, feedURL, "whenever")
	if res.Code != http.StatusBadRequest {
		t.Errorf("POST with a nonsense cadence = %d, want 400", res.Code)
	}

	f, err := tr.store.GetFeed(t.Context(), tr.alice, id)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if f.PollIntervalOverride == nil || *f.PollIntervalOverride != 6*time.Hour {
		t.Errorf("cadence after a refused edit = %v, want the stored 6h", f.PollIntervalOverride)
	}
	// The title was posted alongside it and must not have landed either.
	if f.Title != "Moved Publication" {
		t.Errorf("title = %q; the rest of a refused edit was applied", f.Title)
	}
}

func TestTheSettingsPageStoresTheGeneralCadence(t *testing.T) {
	rd, tr := readingFixture(t)

	page := rd.body("/settings")
	if !strings.Contains(page, `name="poll_every"`) {
		t.Fatalf("the settings page has no cadence picker:\n%s", page)
	}

	rec := rd.do(http.MethodPost, "/settings", url.Values{
		"palette": {""}, "mode": {""}, "poll_every": {"3h"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings = %d\n%s", rec.Code, rec.Body.String())
	}

	prefs, err := tr.store.GetPreferences(t.Context(), tr.alice)
	if err != nil {
		t.Fatalf("GetPreferences() = %v", err)
	}
	if prefs.DefaultPollInterval == nil || *prefs.DefaultPollInterval != 3*time.Hour {
		t.Fatalf("stored preference = %v, want 3h", prefs.DefaultPollInterval)
	}

	// Read back from the database rather than from the form that set it, so a save
	// that failed cannot show as a save that worked.
	if again := rd.body("/settings"); !strings.Contains(again, `value="3h" selected`) {
		t.Errorf("the settings page does not report the stored cadence:\n%s", again)
	}
}

// Read before anything is written, so a cadence that means nothing costs the whole
// save rather than leaving the palette stored and the interval not.
func TestAnUnknownCadenceCostsTheWholeSettingsSave(t *testing.T) {
	rd, tr := readingFixture(t)

	rec := rd.do(http.MethodPost, "/settings", url.Values{
		"palette": {"verdant"}, "mode": {""}, "poll_every": {"often"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /settings with a nonsense cadence = %d, want 400", rec.Code)
	}
	if body := rd.body("/"); strings.Contains(body, `data-theme="verdant"`) {
		t.Error("the palette was stored by a save that was refused")
	}

	prefs, err := tr.store.GetPreferences(t.Context(), tr.alice)
	if err != nil {
		t.Fatalf("GetPreferences() = %v", err)
	}
	if prefs.DefaultPollInterval != nil {
		t.Errorf("a refused cadence was stored as %v", *prefs.DefaultPollInterval)
	}
}

// Turning the preference off again is a choice like any other, and the picker has to
// be able to express it.
func TestTheGeneralCadenceCanBeSetBackToAutomatic(t *testing.T) {
	rd, tr := readingFixture(t)

	for _, value := range []string{"12h", ""} {
		if rec := rd.do(http.MethodPost, "/settings", url.Values{
			"palette": {""}, "mode": {""}, "poll_every": {value},
		}); rec.Code != http.StatusOK {
			t.Fatalf("POST /settings poll_every=%q = %d", value, rec.Code)
		}
	}

	prefs, err := tr.store.GetPreferences(t.Context(), tr.alice)
	if err != nil {
		t.Fatalf("GetPreferences() = %v", err)
	}
	if prefs.DefaultPollInterval != nil {
		t.Errorf("preference after choosing automatic = %v, want nil", *prefs.DefaultPollInterval)
	}
}
