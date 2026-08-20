package store

import (
	"strings"
	"time"
)

// A reader's own answer to "how often should this be checked", which the poller
// otherwise decides for itself.
//
// The adaptive interval is right for most feeds and wrong for the ones somebody
// cares about the timing of: a feed that publishes twice a year climbs to the
// ceiling and stays there, which is correct until the one week a year the reader is
// waiting on it. So there are two explicit settings — one general preference and a
// per-feed override — and this file is the vocabulary they share.

// PollChoice is one cadence, in the form the settings page and the feed form offer
// it.
type PollChoice struct {
	// Value is what the form posts. Empty means automatic.
	Value string

	// Name is the sentence on the option.
	Name string

	// Phrase is the same cadence inside a sentence, for a page reporting what it
	// just stored. Separate from Name rather than derived, because "Once a week"
	// lowercased is not the phrase anybody writes.
	Phrase string

	// Interval is what Value means. Zero for automatic.
	Interval time.Duration
}

// PollChoices are the cadences on offer, in the order they appear in the picker.
//
// Automatic is first because it is what every feed has until somebody chooses
// otherwise. The list runs past TOME_POLL_MAX_INTERVAL on purpose: a reader asking
// for *less* traffic than the ceiling is asking for something this should never
// refuse, and a weekly feed polled daily is 6 requests a week nobody wanted.
var PollChoices = []PollChoice{
	{Value: "", Name: "Automatically", Phrase: "automatically"},
	{Value: "15m", Name: "Every 15 minutes", Phrase: "every 15 minutes", Interval: 15 * time.Minute},
	{Value: "30m", Name: "Every 30 minutes", Phrase: "every 30 minutes", Interval: 30 * time.Minute},
	{Value: "1h", Name: "Once an hour", Phrase: "every hour", Interval: time.Hour},
	{Value: "3h", Name: "Every 3 hours", Phrase: "every 3 hours", Interval: 3 * time.Hour},
	{Value: "6h", Name: "Every 6 hours", Phrase: "every 6 hours", Interval: 6 * time.Hour},
	{Value: "12h", Name: "Every 12 hours", Phrase: "every 12 hours", Interval: 12 * time.Hour},
	{Value: "24h", Name: "Once a day", Phrase: "every day", Interval: 24 * time.Hour},
	{Value: "48h", Name: "Every 2 days", Phrase: "every 2 days", Interval: 48 * time.Hour},
	{Value: "168h", Name: "Once a week", Phrase: "every week", Interval: 168 * time.Hour},
}

// MaxPollChoice is the longest interval that may be stored.
//
// A bound rather than a constraint in the schema, for the reason the migration
// gives, and generous rather than tight: this exists to reject a typo that would
// park a feed past the end of the calendar, not to have an opinion about how quiet
// a feed is allowed to be.
const MaxPollChoice = 366 * 24 * time.Hour

// PollIntervalFor resolves a posted value to the interval it names. Nil is
// automatic; the second return reports whether the value means anything at all.
//
// Parsed rather than matched against PollChoices, which is a deliberate difference
// from how the theme is read. A theme ends up inside an HTML attribute, so only
// values assembled from a known list may reach the database; this ends up as a
// Postgres interval and is never echoed back as anything but an option this package
// generated, so parsing costs nothing and buys two things: a value stored before
// the list changed still round-trips through the form, and so does one an operator
// set by hand.
//
// An unrecognized value is refused rather than quietly read as automatic. A
// cadence that silently reverted to "decide for me" is a preference somebody set,
// was told was saved, and did not get.
func PollIntervalFor(value string) (*time.Duration, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, true
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil || d <= 0 || d > MaxPollChoice {
		return nil, false
	}
	return &d, true
}

// PollChoiceValue is the form value for a stored interval, which is what makes the
// picker come back showing what is stored.
//
// An interval that matches no choice — set by hand, or offered by an older release
// — returns its own duration rather than falling back to automatic, so the form
// reports the truth and a save that changes nothing else leaves it alone.
func PollChoiceValue(d *time.Duration) string {
	if d == nil || *d <= 0 {
		return ""
	}
	for _, c := range PollChoices {
		if c.Interval == *d {
			return c.Value
		}
	}
	return d.String()
}

// PollChoiceFor is the choice a stored interval corresponds to, synthesizing one
// for an interval that is not on the list. The bool reports whether it came from
// PollChoices.
func PollChoiceFor(d *time.Duration) (PollChoice, bool) {
	value := PollChoiceValue(d)
	for _, c := range PollChoices {
		if c.Value == value {
			return c, true
		}
	}
	return PollChoice{Value: value, Name: "Every " + value, Phrase: "every " + value, Interval: *d}, false
}
