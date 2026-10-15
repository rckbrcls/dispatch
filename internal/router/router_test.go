package router

import (
	"math"
	"testing"

	"dispatch/internal/pace"
	"dispatch/internal/stats"
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

func TestBiasPreferAndAvoid(t *testing.T) {
	// Both agents have the same reserve; prefer wins the tie, avoid loses to unknown quota.
	reserve := pace.Pace{Delta: -20, WillLast: true}
	a := Candidate{Model: "a", Pace: &reserve, Bias: AvoidBias}
	b := Candidate{Model: "b", Pace: &reserve, Bias: PreferBias}
	c := Candidate{Model: "c"} // unknown quota
	got := Rank([]Candidate{a, c, b})
	if got[0].Model != "b" || got[1].Model != "c" || got[2].Model != "a" {
		t.Fatalf("order = %s %s %s", got[0].Model, got[1].Model, got[2].Model)
	}
}

func TestBalancedScoreAndReasonUnchanged(t *testing.T) {
	reserve := &pace.Pace{Delta: -30, WillLast: true}
	p50 := int64(1000)
	legacy := Candidate{Model: "m", Pace: reserve, Bias: PreferBias, Hist: &pace.History{Valid: true, Risk: 0.4}, CooledDown: true}
	wantScore, wantReason := Score(legacy), Reason(legacy)
	legacy.Priority = PriorityBalanced
	legacy.Speed, legacy.Strength = 1, 5
	legacy.Evidence = stats.ModelEvidence{Samples: 8, DurationN: 8, Verified: 8, VerifiedRate: 1, P50Ms: &p50}
	if Score(legacy) != wantScore || Reason(legacy) != wantReason {
		t.Fatalf("score %v want %v\n%s\n%s", Score(legacy), wantScore, Reason(legacy), wantReason)
	}
	out := &pace.Pace{Delta: 30, Deficit: true}
	little := &pace.Pace{Delta: -5, WillLast: true}
	deficit := &pace.Pace{Delta: 10, Deficit: true, WillLast: true}
	cs := []Candidate{{Model: "out", Pace: out}, {Model: "unk"}, {Model: "deficit", Pace: deficit}, {Model: "little", Pace: little}, {Model: "reserve", Pace: reserve}}
	for i := range cs {
		cs[i].Priority = PriorityBalanced
		cs[i].Speed = 5
		cs[i].Evidence = legacy.Evidence
	}
	got := Rank(cs)
	want := []string{"reserve", "little", "unk", "deficit", "out"}
	for i, w := range want {
		if got[i].Model != w {
			t.Fatalf("pos %d = %s, want %s", i, got[i].Model, w)
		}
	}
}

func TestSpeedAndQualityDiverge(t *testing.T) {
	fast := Candidate{Model: "fast", Speed: 5, Strength: 1, Priority: PrioritySpeed}
	strong := Candidate{Model: "strong", Speed: 1, Strength: 5, Priority: PrioritySpeed}
	if Rank([]Candidate{strong, fast})[0].Model != "fast" {
		t.Fatal("speed")
	}
	fast.Priority, strong.Priority = PriorityQuality, PriorityQuality
	if Rank([]Candidate{fast, strong})[0].Model != "strong" {
		t.Fatal("quality")
	}
	fast.Priority, strong.Priority = PriorityBalanced, PriorityBalanced
	got := Rank([]Candidate{fast, strong})
	if got[0].Model != "fast" || got[1].Model != "strong" {
		t.Fatalf("balanced %s %s", got[0].Model, got[1].Model)
	}
}

func TestEvidenceOverridesPriorOnlyAfterMinSamples(t *testing.T) {
	slow := int64(600_000)
	quick := int64(1000)
	speedWinner := func(n int) string {
		a := Candidate{Model: "fast", Priority: PrioritySpeed, Speed: 1, Evidence: stats.ModelEvidence{DurationN: n, P50Ms: &quick}}
		b := Candidate{Model: "strong", Priority: PrioritySpeed, Speed: 5, Evidence: stats.ModelEvidence{DurationN: n, P50Ms: &slow}}
		return Rank([]Candidate{a, b})[0].Model
	}
	if speedWinner(4) != "strong" || speedWinner(5) != "fast" {
		t.Fatalf("speed prior %s measured %s", speedWinner(4), speedWinner(5))
	}
	qualityWinner := func(n int) string {
		a := Candidate{Model: "weak", Priority: PriorityQuality, Strength: 5, Evidence: stats.ModelEvidence{Samples: n, VerifiedRate: 0}}
		b := Candidate{Model: "proven", Priority: PriorityQuality, Strength: 1, Evidence: stats.ModelEvidence{Samples: n, VerifiedRate: 1}}
		return Rank([]Candidate{a, b})[0].Model
	}
	if qualityWinner(4) != "weak" || qualityWinner(5) != "proven" {
		t.Fatalf("quality prior %s measured %s", qualityWinner(4), qualityWinner(5))
	}
}

func TestPriorityDoesNotOutrankCooldown(t *testing.T) {
	quick := int64(1000)
	slow := int64(600_000)
	cooled := Candidate{
		Model: "cooled", Priority: PrioritySpeed, Speed: 5, CooledDown: true,
		Pace:     &pace.Pace{Delta: -50, WillLast: true},
		Evidence: stats.ModelEvidence{DurationN: 20, Samples: 20, VerifiedRate: 1, P50Ms: &quick},
	}
	other := Candidate{
		Model: "other", Priority: PrioritySpeed, Speed: 1,
		Pace:     &pace.Pace{Delta: 80, Deficit: true, WillLast: true},
		Evidence: stats.ModelEvidence{DurationN: 20, Samples: 20, P50Ms: &slow},
	}
	if Rank([]Candidate{cooled, other})[0].Model != "other" {
		t.Fatal("cooldown pierced")
	}
}

func TestTermsSumToScore(t *testing.T) {
	p50 := int64(1000)
	cases := []Candidate{
		{Priority: PrioritySpeed, Speed: 4, Pace: &pace.Pace{Delta: -10, WillLast: true}, Bias: PreferBias, Evidence: stats.ModelEvidence{DurationN: 6, P50Ms: &p50}},
		{Priority: PriorityQuality, Strength: 2, Evidence: stats.ModelEvidence{Samples: 6, VerifiedRate: 0.5}},
		{Priority: PriorityBalanced, Pace: &pace.Pace{Delta: 4, Deficit: true, WillLast: true}, Hist: &pace.History{Valid: true, Risk: 0.25}},
	}
	for _, c := range cases {
		sum := 0.0
		for _, term := range Terms(c) {
			sum += term.Value
		}
		if math.Abs(sum-Score(c)) > 1e-9 {
			t.Fatalf("%s terms %v sum %v score %v", c.Priority, Terms(c), sum, Score(c))
		}
	}
}
