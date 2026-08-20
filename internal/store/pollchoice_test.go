package store_test

import (
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The vocabulary the two cadence pickers share. No database: these are the
// functions that decide what a posted form value means, and getting them wrong
// stores a cadence nobody chose.

func TestPollIntervalForAcceptsTheChoicesOnOffer(t *testing.T) {
	for _, c := range store.PollChoices {
		got, ok := store.PollIntervalFor(c.Value)
		if !ok {
			t.Errorf("PollIntervalFor(%q) refused a value the picker offers", c.Value)
			continue
		}
		switch {
		case c.Interval == 0:
			if got != nil {
				t.Errorf("PollIntervalFor(%q) = %v, want nil for automatic", c.Value, *got)
			}
		case got == nil:
			t.Errorf("PollIntervalFor(%q) = nil, want %v", c.Value, c.Interval)
		case *got != c.Interval:
			t.Errorf("PollIntervalFor(%q) = %v, want %v", c.Value, *got, c.Interval)
		}
	}
}

func TestPollIntervalForRefusesWhatItCannotStore(t *testing.T) {
	// Refused rather than read as automatic. A cadence that silently reverted to
	// "decide for me" is a preference somebody set, was told was saved, and did not
	// get.
	for _, value := range []string{
		"soon", "15", "-1h", "0", "0s", "1h30x", "999999h", "'; DROP TABLE feeds --",
	} {
		if _, ok := store.PollIntervalFor(value); ok {
			t.Errorf("PollIntervalFor(%q) was accepted", value)
		}
	}
}

// A value nobody typed into the picker still round-trips, which is what keeps an
// interval set by hand — or by an older release — from being overwritten by the
// next unrelated save.
func TestOffListIntervalsRoundTrip(t *testing.T) {
	odd := 47 * time.Minute

	value := store.PollChoiceValue(&odd)
	got, ok := store.PollIntervalFor(value)
	if !ok {
		t.Fatalf("PollIntervalFor(%q) refused what PollChoiceValue produced", value)
	}
	if got == nil || *got != odd {
		t.Fatalf("round trip of %v produced %v", odd, got)
	}

	choice, listed := store.PollChoiceFor(&odd)
	if listed {
		t.Errorf("PollChoiceFor(%v) claimed to be one of the offered choices", odd)
	}
	if choice.Value != value {
		t.Errorf("synthesized choice value = %q, want %q", choice.Value, value)
	}
	if choice.Name == "" || choice.Phrase == "" {
		t.Errorf("synthesized choice has nothing to draw: %+v", choice)
	}
}

func TestPollChoiceValueIsEmptyForAutomatic(t *testing.T) {
	if got := store.PollChoiceValue(nil); got != "" {
		t.Errorf("PollChoiceValue(nil) = %q, want the empty automatic value", got)
	}

	// A zero interval is not a cadence; it is the absence of one arriving as a
	// value rather than as nil.
	var zero time.Duration
	if got := store.PollChoiceValue(&zero); got != "" {
		t.Errorf("PollChoiceValue(0) = %q, want the empty automatic value", got)
	}
}

// Every offered value has to survive the form: stored, read back, and selected
// again without becoming a different option.
func TestEveryChoiceSelectsItselfAgain(t *testing.T) {
	for _, c := range store.PollChoices {
		d, ok := store.PollIntervalFor(c.Value)
		if !ok {
			t.Fatalf("PollIntervalFor(%q) refused an offered value", c.Value)
		}
		if got := store.PollChoiceValue(d); got != c.Value {
			t.Errorf("%q stored and read back as %q", c.Value, got)
		}
		if _, listed := store.PollChoiceFor(d); !listed && c.Value != "" {
			t.Errorf("PollChoiceFor did not recognize the offered choice %q", c.Value)
		}
	}
}
