package classify

import (
	"math"
	"sort"

	"dispatch/internal/adapter"
	"dispatch/internal/store"
)

const (
	MinSamples   = 5
	learningRate = 0.02
	l2           = 0.001
	maxWeight    = 12.0
	Threshold    = 0.6
)

type TierModel struct {
	Weights Features `json:"weights"`
	Samples uint64   `json:"samples"`
}

// Model stores only coefficients, schema and counts; no prompts or examples.
// Separate heads prevent easy-tier outcomes from training the hard tier.
type Model struct {
	FeatureSchema int                        `json:"featureSchema"`
	Tiers         map[adapter.Tier]TierModel `json:"tiers"`
}

func Prior() Model {
	m := Model{FeatureSchema: featureSchema, Tiers: map[adapter.Tier]TierModel{}}
	for _, t := range []adapter.Tier{adapter.TierEasy, adapter.TierNormal, adapter.TierHard} {
		var w Features
		w[0] = -2
		for i := 13; i < 18; i++ {
			w[i] = 4
		}
		m.Tiers[t] = TierModel{Weights: w}
	}
	return m
}

func clamp(x, lo, hi float64) float64 {
	if math.IsNaN(x) {
		return 0
	}
	return math.Max(lo, math.Min(hi, x))
}

func clean(f Features) Features {
	for i := range f {
		f[i] = clamp(f[i], 0, 1)
	}
	f[0] = 1
	return f
}

func probability(w, f Features) float64 {
	z := 0.0
	for i, x := range clean(f) {
		z += clamp(w[i], -maxWeight, maxWeight) * x
	}
	return 1 / (1 + math.Exp(-clamp(z, -30, 30)))
}

type Contribution struct {
	Feature      string  `json:"feature"`
	Value        float64 `json:"value"`
	Weight       float64 `json:"weight"`
	Contribution float64 `json:"contribution"`
}

type Prediction struct {
	RequestedTier adapter.Tier   `json:"requestedTier"`
	SuggestedTier adapter.Tier   `json:"suggestedTier"`
	Score         float64        `json:"score"` // P(verification_failed | requested tier, features)
	Samples       uint64         `json:"samples"`
	FeatureSchema int            `json:"featureSchema"`
	Reasons       []Contribution `json:"reasons"`
}

// Predict only recommends one tier upward. The score is not calibrated until
// enough representative verifier outcomes have been observed in this tier.
func (m Model) Predict(t adapter.Tier, f Features, explain bool) Prediction {
	h, ok := m.Tiers[t]
	if !ok || m.FeatureSchema != featureSchema {
		h = Prior().Tiers[t]
	}
	f = clean(f)
	r := Prediction{RequestedTier: t, SuggestedTier: t, Score: probability(h.Weights, f), Samples: h.Samples, FeatureSchema: featureSchema, Reasons: []Contribution{}}
	if r.Score >= Threshold {
		r.SuggestedTier = Higher(t)
	}
	for i, x := range f {
		w := clamp(h.Weights[i], -maxWeight, maxWeight)
		if explain || (x != 0 && w != 0) {
			r.Reasons = append(r.Reasons, Contribution{names[i], x, w, x * w})
		}
	}
	sort.SliceStable(r.Reasons, func(i, j int) bool { return math.Abs(r.Reasons[i].Contribution) > math.Abs(r.Reasons[j].Contribution) })
	if !explain && len(r.Reasons) > 5 {
		r.Reasons = r.Reasons[:5]
	}
	return r
}

// Learn performs at most one SGD step, beginning with the fifth eligible
// example in that tier. Warmup keeps only counts, never buffered prompts.
func (m *Model) Learn(t adapter.Tier, f Features, failed bool) {
	if !t.Valid() {
		return
	}
	if m.FeatureSchema != featureSchema || !m.valid() {
		*m = Prior()
	}
	h := m.Tiers[t]
	if h.Samples < ^uint64(0) {
		h.Samples++
	}
	if h.Samples >= MinSamples {
		f = clean(f)
		y := 0.0
		if failed {
			y = 1
		}
		err := probability(h.Weights, f) - y
		for i, x := range f {
			penalty := l2 * h.Weights[i]
			if i == 0 {
				penalty = 0 // do not regularize the intercept
			}
			h.Weights[i] = clamp(h.Weights[i]-learningRate*(err*x+penalty), -maxWeight, maxWeight)
		}
	}
	m.Tiers[t] = h
}

// Label accepts only a completed, attributable verifier verdict. Failed
// fallback attempts do not erase a later successful, verified execution.
func Label(r store.Record) (failed, eligible bool) {
	if r.Status != store.Completed || r.NoVerify || r.ReadOnly {
		return false, false
	}
	if r.ExitClass != "" && r.ExitClass != "ok" && r.ExitClass != "verify_fail" {
		return false, false
	}
	if r.Verify != nil {
		if string(r.Verify.Status) != r.Verification {
			return false, false
		}
		for _, c := range r.Verify.Checks {
			if c.Status == "timeout" || c.Status == "error" || c.Name == "conflicts" && len(c.Regressions) > 0 {
				return false, false
			}
		}
	}
	switch r.Verification {
	case "verified":
		return false, true
	case "verification_failed":
		return true, true
	default:
		return false, false
	}
}

func (m Model) valid() bool {
	if m.FeatureSchema != featureSchema || len(m.Tiers) != 3 {
		return false
	}
	for _, t := range []adapter.Tier{adapter.TierEasy, adapter.TierNormal, adapter.TierHard} {
		h, ok := m.Tiers[t]
		if !ok {
			return false
		}
		for _, w := range h.Weights {
			if math.IsNaN(w) || math.IsInf(w, 0) || math.Abs(w) > maxWeight {
				return false
			}
		}
	}
	return true
}
