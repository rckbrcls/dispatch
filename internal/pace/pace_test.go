package pace

import (
	"testing"
	"time"
)

func TestLinear(t *testing.T) {
	week := 7 * 24 * time.Hour
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(week / 2) // halfway through the window

	cases := []struct {
		name     string
		used     float64
		deficit  bool
		willLast bool
		stage    Stage
	}{
		{"on track", 50, false, true, OnTrack},
		{"reserve", 20, false, true, Far},
		{"deficit runs out", 80, true, false, Far},
		{"slightly ahead", 54, true, false, Slightly},
	}
	for _, c := range cases {
		p := Linear(c.used, week, reset, now)
		if p.Deficit != c.deficit || p.WillLast != c.willLast || p.Stage != c.stage {
			t.Errorf("%s: got %+v", c.name, p)
		}
	}
}

func TestLinearRunsOutIn(t *testing.T) {
	week := 7 * 24 * time.Hour
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	p := Linear(80, week, now.Add(week/2), now)
	// 80% in 3.5d -> 20% left lasts 0.875d
	want := time.Duration(0.875 * float64(24*time.Hour))
	if d := p.RunsOutIn - want; d < -time.Second || d > time.Second {
		t.Errorf("RunsOutIn = %v, want %v", p.RunsOutIn, want)
	}
}
