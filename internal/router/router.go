// Package router picks which adapter/model serves a tier.
package router

import (
	"sort"
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/pace"
)

// Candidate is one adapter/model able to serve the requested tier.
type Candidate struct {
	Adapter adapter.Adapter
	Model   string
	Quota   *adapter.Quota // nil when unknown
	Pace    *pace.Pace
	Hist    *pace.History // nil unless >=5 complete weeks are on record
	// CooledDown marks an agent that recently hit a rate limit.
	CooledDown bool
	// Bias is a soft preference added to the score (negative = preferred).
	Bias float64
}

// Rank orders candidates: known reserve first (most reserve on top), then
// unknown quota, then deficit that still lasts, then deficit that runs out.
func Rank(cs []Candidate) []Candidate {
	out := append([]Candidate(nil), cs...)
	sort.SliceStable(out, func(i, j int) bool { return score(out[i]) < score(out[j]) })
	return out
}

func score(c Candidate) float64 {
	var base float64
	switch {
	case c.Pace == nil:
		base = 1000 // unknown: after reserve, before deficit
	case !c.Pace.WillLast:
		base = 3000 + c.Pace.Delta
	case c.Pace.Deficit:
		base = 2000 + c.Pace.Delta
	default:
		base = c.Pace.Delta // negative = reserve, more negative first
	}
	if c.Hist != nil && c.Hist.Valid {
		base += c.Hist.Risk * 40 // history: how often this pace ran dry
	}
	if c.CooledDown {
		base += 10000
	}
	return base + c.Bias
}

// Evaluate fills Pace from Quota at the given time.
func Evaluate(c *Candidate, now time.Time) {
	if c.Quota == nil {
		return
	}
	w := time.Duration(c.Quota.WindowMinutes) * time.Minute
	p := pace.Linear(c.Quota.UsedPercent, w, c.Quota.ResetsAt, now)
	c.Pace = &p
}

// Bias values for subagent profiles. Preferring breaks ties inside a pace
// band; avoiding pushes an agent past unknown quota but not past deficit.
const (
	PreferBias = -50
	AvoidBias  = 1500
)
