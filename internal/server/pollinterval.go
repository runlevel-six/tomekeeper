package server

import (
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The cadence picker, shared by the settings page — where it sets the reader's
// general preference — and by the feed form, where it overrides that preference for
// one subscription.

// pollChoices is the option list for a picker currently showing `current`.
//
// Choices shorter than this instance's floor are left out. TOME_POLL_MIN_INTERVAL
// is a promise made to other people's servers, so offering "every 15 minutes" on an
// instance whose floor is an hour would be offering something that cannot happen —
// the poller raises it, and the reader is never told.
//
// Whatever is stored is always on the list, even when it is one of those. A picker
// that silently dropped the stored value would report the wrong cadence, and saving
// an unrelated change on the same form would then quietly overwrite it with
// whatever the browser had selected instead.
func (s *Server) pollChoices(current string) []store.PollChoice {
	var floor time.Duration
	if s.cfg != nil {
		floor = s.cfg.PollMinInterval
	}

	extra, hasExtra := offListChoice(current)
	out := make([]store.PollChoice, 0, len(store.PollChoices)+1)

	for _, c := range store.PollChoices {
		// Kept in ascending order, which is where an off-list value has to be
		// inserted rather than appended: a picker running 30 minutes, 1 hour, 45
		// minutes reads as a bug in the page.
		if hasExtra && extra.Interval < c.Interval {
			out = append(out, extra)
			hasExtra = false
		}
		if c.Value != current && c.Interval > 0 && c.Interval < floor {
			continue
		}
		out = append(out, c)
	}
	if hasExtra {
		out = append(out, extra)
	}
	return out
}

// offListChoice is the synthesized option for a stored interval that PollChoices
// does not contain — one set by hand, or offered by an older release.
func offListChoice(current string) (store.PollChoice, bool) {
	if current == "" {
		return store.PollChoice{}, false
	}
	d, ok := store.PollIntervalFor(current)
	if !ok || d == nil {
		return store.PollChoice{}, false
	}
	choice, listed := store.PollChoiceFor(d)
	return choice, !listed
}

// cadencePhrase says how often a feed will be checked from now on, as the tail of
// a sentence beginning "it will be checked".
//
// Three cases rather than one, because "every 3 hours" is the answer to a different
// question depending on where the 3 hours came from: a reader who has just cleared
// an override needs to be told which setting took over, or the page reads as though
// their change was ignored.
func cadencePhrase(f store.Feed) string {
	switch {
	case f.PollIntervalOverride != nil:
		choice, _ := store.PollChoiceFor(f.PollIntervalOverride)
		return choice.Phrase
	case f.DefaultPollInterval != nil:
		choice, _ := store.PollChoiceFor(f.DefaultPollInterval)
		return choice.Phrase + ", which is your setting for every feed"
	default:
		return "as often as it looks worth checking"
	}
}

// pollFloorLabel is this instance's floor as prose, for the line that tells a
// reader a shorter choice would be raised to it.
//
// Durations print as "15m0s", which is not a thing to put in a sentence.
func (s *Server) pollFloorLabel() string {
	if s.cfg == nil {
		return ""
	}
	d := s.cfg.PollMinInterval
	switch {
	case d <= 0:
		return ""
	case d%time.Hour == 0:
		return plural(int(d.Hours()), "hour")
	case d%time.Minute == 0:
		return plural(int(d.Minutes()), "minute")
	default:
		return d.String()
	}
}
