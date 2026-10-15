package pace

import (
	"testing"
	"time"
)

// weekSnaps fabricates a complete week ending at end where usage ramps
// linearly to finalPct, sampled every 12 hours.
func weekSnaps(end time.Time, finalPct float64) []Snapshot {
	var out []Snapshot
	start := end.Add(-week)
	for h := 0; h <= 168; h += 12 {
		t := start.Add(time.Duration(h) * time.Hour)
		out = append(out, Snapshot{
			UsedPercent: finalPct * float64(h) / 168, ResetsAt: end,
			WindowMinutes: int(week / time.Minute), SampledAt: t,
		})
	}
	return out
}

func TestHistoricalNeedsFiveWeeks(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	reset := now.Add(3 * 24 * time.Hour)
	var snaps []Snapshot
	for i := 1; i <= 4; i++ {
		snaps = append(snaps, weekSnaps(reset.Add(-time.Duration(i)*week), 100)...)
	}
	if h := Historical(snaps, 50, reset, now); h.Valid || h.Weeks != 4 {
		t.Fatalf("%+v", h)
	}
}

func TestHistoricalRisk(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	reset := now.Add(3 * 24 * time.Hour) // 4 of 7 days elapsed
	build := func(final float64) []Snapshot {
		var s []Snapshot
		for i := 1; i <= 6; i++ {
			s = append(s, weekSnaps(reset.Add(-time.Duration(i)*week), final)...)
		}
		return s
	}
	// Heavy history, already ahead of the typical curve: crossing is likely.
	heavy := Historical(build(100), 80, reset, now)
	if !heavy.Valid || heavy.Weeks != 6 || heavy.Risk < 0.8 {
		t.Fatalf("heavy: %+v", heavy)
	}
	if heavy.RunsOutIn <= 0 || heavy.RunsOutIn > 3*24*time.Hour {
		t.Fatalf("runsOutIn %v", heavy.RunsOutIn)
	}
	// Light history: shifting to current usage still never crosses.
	light := Historical(build(30), 20, reset, now)
	if light.Risk > 0.2 || light.RunsOutIn != 0 {
		t.Fatalf("light: %+v", light)
	}
}
