package router

import (
	"testing"

	"dispatch/internal/pace"
)

func TestRankOrder(t *testing.T) {
	reserve := &pace.Pace{Delta: -30, WillLast: true}
	little := &pace.Pace{Delta: -5, WillLast: true}
	deficit := &pace.Pace{Delta: 10, Deficit: true, WillLast: true}
	out := &pace.Pace{Delta: 30, Deficit: true}
	got := Rank([]Candidate{{Model: "out", Pace: out}, {Model: "unk"}, {Model: "deficit", Pace: deficit}, {Model: "little", Pace: little}, {Model: "reserve", Pace: reserve}})
	want := []string{"reserve", "little", "unk", "deficit", "out"}
	for i, w := range want {
		if got[i].Model != w {
			t.Fatalf("pos %d = %s, want %s", i, got[i].Model, w)
		}
	}
}

func TestRankCooldownAndRisk(t *testing.T) {
	calm := &pace.Pace{Delta: -10, WillLast: true}
	got := Rank([]Candidate{
		{Model: "cooled", Pace: calm, CooledDown: true},
		{Model: "risky", Pace: calm, Hist: &pace.History{Valid: true, Risk: 0.9}},
		{Model: "safe", Pace: calm, Hist: &pace.History{Valid: true, Risk: 0.1}},
	})
	want := []string{"safe", "risky", "cooled"}
	for i, w := range want {
		if got[i].Model != w {
			t.Fatalf("pos %d = %s, want %s", i, got[i].Model, w)
		}
	}
}
