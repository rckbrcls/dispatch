// Package pace computes linear quota pace for a usage window.
package pace

import "time"

// Stage buckets |Delta| the way CodexBar does.
type Stage string

const (
	OnTrack  Stage = "on_track"
	Slightly Stage = "slightly"
	Ahead    Stage = "ahead"
	Far      Stage = "far"
)

// Pace is the result of comparing actual usage to the linear expectation.
type Pace struct {
	Expected float64 // percent expected used by now
	Actual   float64 // percent actually used
	// Delta > 0 means deficit (using faster than linear), < 0 means reserve.
	Delta     float64
	Stage     Stage
	Deficit   bool
	WillLast  bool          // projected to last until reset
	RunsOutIn time.Duration // valid when !WillLast
}

// Linear computes pace for a window of the given length that resets at
// resetsAt, with usedPercent consumed so far.
func Linear(usedPercent float64, window time.Duration, resetsAt, now time.Time) Pace {
	remaining := resetsAt.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	elapsed := window - remaining
	p := Pace{Actual: usedPercent}
	if window <= 0 {
		p.WillLast = true
		return p
	}
	if elapsed < 0 {
		elapsed = 0
	}
	p.Expected = float64(elapsed) / float64(window) * 100
	p.Delta = usedPercent - p.Expected
	p.Deficit = p.Delta > 0
	p.Stage = stage(p.Delta)

	if usedPercent <= 0 || elapsed == 0 {
		p.WillLast = true
		return p
	}
	rate := usedPercent / float64(elapsed) // percent per ns
	eta := time.Duration((100 - usedPercent) / rate)
	if usedPercent >= 100 {
		eta = 0
	}
	if eta >= remaining {
		p.WillLast = true
	} else {
		p.RunsOutIn = eta
	}
	return p
}

func stage(delta float64) Stage {
	if delta < 0 {
		delta = -delta
	}
	switch {
	case delta <= 2:
		return OnTrack
	case delta <= 6:
		return Slightly
	case delta <= 12:
		return Ahead
	default:
		return Far
	}
}
