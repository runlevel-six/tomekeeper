package feed_test

import (
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/feed"
)

func TestClamp(t *testing.T) {
	p := feed.DefaultIntervalPolicy()

	tests := []struct {
		in, want time.Duration
	}{
		{time.Minute, 15 * time.Minute},
		{0, 15 * time.Minute},
		{-time.Hour, 15 * time.Minute},
		{15 * time.Minute, 15 * time.Minute},
		{time.Hour, time.Hour},
		{24 * time.Hour, 24 * time.Hour},
		{48 * time.Hour, 24 * time.Hour},
	}
	for _, tt := range tests {
		if got := p.Clamp(tt.in); got != tt.want {
			t.Errorf("Clamp(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestOnNoChangeGrowsToCeiling(t *testing.T) {
	p := feed.DefaultIntervalPolicy()

	interval := p.Min
	for range 100 {
		next := p.OnNoChange(interval)
		if next < interval {
			t.Fatalf("OnNoChange(%v) = %v, which is shorter — a quiet feed must not be polled more often", interval, next)
		}
		interval = next
	}
	if interval != p.Max {
		t.Errorf("after 100 quiet polls interval = %v, want the ceiling %v", interval, p.Max)
	}
}

func TestOnNewItemsShrinksToFloor(t *testing.T) {
	p := feed.DefaultIntervalPolicy()

	interval := p.Max
	for range 100 {
		next := p.OnNewItems(interval)
		if next > interval {
			t.Fatalf("OnNewItems(%v) = %v, which is longer — a busy feed must not be polled less often", interval, next)
		}
		interval = next
	}
	if interval != p.Min {
		t.Errorf("after 100 productive polls interval = %v, want the floor %v", interval, p.Min)
	}
}

// A feed that alternates between publishing and not must not drift to an
// extreme; the interval should hover around the feed's real cadence.
func TestIntervalConvergesForAlternatingFeed(t *testing.T) {
	p := feed.DefaultIntervalPolicy()

	interval := p.Min
	for range 200 {
		interval = p.OnNoChange(interval)
		interval = p.OnNewItems(interval)
	}

	if interval < p.Min || interval > p.Max {
		t.Errorf("interval %v escaped [%v, %v]", interval, p.Min, p.Max)
	}
	// 1.5 up then halve is a net 0.75 per cycle, so an alternating feed should
	// settle at the floor rather than climbing.
	if interval != p.Min {
		t.Errorf("alternating feed settled at %v, want the floor %v", interval, p.Min)
	}
}

func TestOnFailureBacksOffExponentially(t *testing.T) {
	p := feed.DefaultIntervalPolicy()

	tests := []struct {
		failures int
		want     time.Duration
	}{
		{0, 15 * time.Minute},  // defensive: treated as the first failure
		{1, 15 * time.Minute},  // floor
		{2, 30 * time.Minute},  // 15m × 2
		{3, time.Hour},         // 15m × 4
		{4, 2 * time.Hour},     // 15m × 8
		{5, 4 * time.Hour},     // 15m × 16
		{6, 8 * time.Hour},     // 15m × 32
		{7, 16 * time.Hour},    // 15m × 64
		{8, 24 * time.Hour},    // would be 32h; capped
		{20, 24 * time.Hour},   // at the disable threshold, still capped
		{1000, 24 * time.Hour}, // no overflow
	}
	for _, tt := range tests {
		if got := p.OnFailure(tt.failures); got != tt.want {
			t.Errorf("OnFailure(%d) = %v, want %v", tt.failures, got, tt.want)
		}
	}
}

// A large failure count must not overflow the float conversion into a negative
// or absurd duration; a feed that has failed for months would otherwise be
// scheduled in the past and polled in a tight loop.
func TestOnFailureNeverOverflows(t *testing.T) {
	p := feed.DefaultIntervalPolicy()

	for _, failures := range []int{31, 32, 33, 63, 64, 65, 1 << 20, 1 << 30} {
		got := p.OnFailure(failures)
		if got <= 0 || got > p.Max {
			t.Errorf("OnFailure(%d) = %v, want a positive duration no greater than %v", failures, got, p.Max)
		}
	}
}

func TestFromHint(t *testing.T) {
	p := feed.DefaultIntervalPolicy()

	tests := []struct {
		period, frequency string
		want              time.Duration
		wantOK            bool
	}{
		{"hourly", "", time.Hour, true},
		{"hourly", "1", time.Hour, true},
		{"hourly", "2", 30 * time.Minute, true},
		{"hourly", "4", 15 * time.Minute, true},
		{"hourly", "60", 15 * time.Minute, true}, // clamped to the floor
		{"daily", "", 24 * time.Hour, true},
		{"daily", "2", 12 * time.Hour, true},
		{"weekly", "", 24 * time.Hour, true},  // clamped to the ceiling
		{"monthly", "", 24 * time.Hour, true}, // clamped to the ceiling
		{"yearly", "", 24 * time.Hour, true},  // clamped to the ceiling
		{"HOURLY", "", time.Hour, true},       // case-insensitive
		{"  daily  ", "", 24 * time.Hour, true},
		{"", "", 0, false},
		{"fortnightly", "", 0, false}, // not in the specification
		{"hourly", "0", time.Hour, true},
		{"hourly", "-1", time.Hour, true},
		{"hourly", "many", time.Hour, true},
	}

	for _, tt := range tests {
		got, ok := p.FromHint(tt.period, tt.frequency)
		if ok != tt.wantOK {
			t.Errorf("FromHint(%q, %q) ok = %v, want %v", tt.period, tt.frequency, ok, tt.wantOK)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("FromHint(%q, %q) = %v, want %v", tt.period, tt.frequency, got, tt.want)
		}
	}
}

// Every path must respect the bounds, whatever it is given.
func TestPolicyAlwaysReturnsBoundedInterval(t *testing.T) {
	p := feed.DefaultIntervalPolicy()

	inputs := []time.Duration{
		-time.Hour, 0, time.Nanosecond, time.Second, p.Min, time.Hour,
		p.Max, 100 * p.Max, time.Duration(1<<62 - 1),
	}
	for _, in := range inputs {
		for name, got := range map[string]time.Duration{
			"OnNewItems": p.OnNewItems(in),
			"OnNoChange": p.OnNoChange(in),
		} {
			if got < p.Min || got > p.Max {
				t.Errorf("%s(%v) = %v, outside [%v, %v]", name, in, got, p.Min, p.Max)
			}
		}
	}
}
