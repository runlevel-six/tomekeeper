package feed

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// IntervalPolicy decides how long to wait before polling a feed again.
//
// The goal is to spend requests where they produce articles. A feed that
// publishes hourly should be polled often; a personal blog that publishes
// twice a year should not be fetched 17,000 times between posts. Since feeds
// do not announce their cadence reliably, the interval is learned from
// observed behavior and corrected whenever the feed proves the estimate wrong.
type IntervalPolicy struct {
	// Min is the floor. No feed is polled more often than this.
	Min time.Duration

	// Max is the ceiling for a quiet feed.
	Max time.Duration

	// Growth multiplies the interval each time a poll finds nothing new.
	Growth float64
}

// DefaultIntervalPolicy matches the poller design: a 15-minute floor, a 24-hour ceiling,
// and 1.5× growth while a feed is quiet.
func DefaultIntervalPolicy() IntervalPolicy {
	return IntervalPolicy{
		Min:    15 * time.Minute,
		Max:    24 * time.Hour,
		Growth: 1.5,
	}
}

// Clamp constrains d to [Min, Max].
func (p IntervalPolicy) Clamp(d time.Duration) time.Duration {
	switch {
	case d < p.Min:
		return p.Min
	case d > p.Max:
		return p.Max
	default:
		return d
	}
}

// Chosen returns the interval to use for a cadence the reader asked for.
//
// Clamped upward to the floor and not downward to the ceiling, which is the one
// asymmetry in this file and is deliberate. The floor is politeness and belongs to
// the operator: TOME_POLL_MIN_INTERVAL is a promise made to other people's servers,
// and a reader cannot spend somebody else's request budget by picking a number in a
// dropdown. The ceiling is only ever this policy protecting a quiet feed from being
// polled pointlessly, so a reader asking for *less* often than the ceiling is
// asking for something there was never a reason to refuse.
func (p IntervalPolicy) Chosen(d time.Duration) time.Duration {
	if d < p.Min {
		return p.Min
	}
	return d
}

// OnNewItems returns the next interval after a poll that found new items.
//
// The interval is halved rather than reset to the floor. "Reset to floor"
// makes every feed that posts once a day climb the whole ladder back to the
// ceiling every day — roughly a dozen wasted requests per feed per day, which
// is precisely the impoliteness the interval exists to avoid. Halving
// converges on the feed's real cadence from both directions instead.
func (p IntervalPolicy) OnNewItems(current time.Duration) time.Duration {
	if current <= 0 {
		return p.Min
	}
	return p.Clamp(current / 2)
}

// OnNoChange returns the next interval after a poll that found nothing new,
// including a 304.
func (p IntervalPolicy) OnNoChange(current time.Duration) time.Duration {
	if current <= 0 {
		return p.Min
	}
	grown := time.Duration(float64(current) * p.Growth)
	// Guard against a Growth of 1.0 or less leaving a short interval stuck.
	if grown <= current {
		grown = current + time.Minute
	}
	return p.Clamp(grown)
}

// OnFailure returns the next interval after a failed poll.
//
// Backoff is exponential in the number of consecutive failures, starting from
// the floor rather than the current interval: a feed that has been failing is
// not a feed whose publishing cadence we have any information about.
//
// There is no jitter. Feeds are already spread across the clock by their own
// next_poll_at values, so there is no synchronized herd to break up, and a
// deterministic interval is one that can be tested and reasoned about when a
// feed's health is being investigated.
func (p IntervalPolicy) OnFailure(consecutiveFailures int) time.Duration {
	if consecutiveFailures < 1 {
		consecutiveFailures = 1
	}
	// Cap the exponent before it can overflow the float conversion.
	const maxExponent = 32
	exponent := min(consecutiveFailures-1, maxExponent)

	backoff := float64(p.Min) * math.Pow(2, float64(exponent))
	if backoff > float64(p.Max) || math.IsInf(backoff, 0) {
		return p.Max
	}
	return p.Clamp(time.Duration(backoff))
}

// FromHint converts a feed's declared update cadence into an interval,
// reporting whether the feed declared one at all.
//
// RSS feeds may advertise their cadence with the syndication module's
// sy:updatePeriod and sy:updateFrequency. Where a feed says how often it
// changes, believe it — but still clamp: the declaration is a publisher's
// aspiration, and "hourly" from a feed that has not changed in a year is not a
// reason to poll it hourly forever.
func (p IntervalPolicy) FromHint(period string, frequency string) (time.Duration, bool) {
	var base time.Duration
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "hourly":
		base = time.Hour
	case "daily":
		base = 24 * time.Hour
	case "weekly":
		base = 7 * 24 * time.Hour
	case "monthly":
		base = 30 * 24 * time.Hour
	case "yearly":
		base = 365 * 24 * time.Hour
	default:
		return 0, false
	}

	// updateFrequency is "how many times per updatePeriod", defaulting to 1.
	times := 1
	if f := strings.TrimSpace(frequency); f != "" {
		parsed, err := strconv.Atoi(f)
		if err == nil && parsed > 0 {
			times = parsed
		}
	}

	return p.Clamp(base / time.Duration(times)), true
}
