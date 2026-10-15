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
}

// Rank orders candidates: known reserve first (most reserve on top), then
// unknown quota, then deficit that still lasts, then deficit that runs out.
func Rank(cs []Candidate) []Candidate {
	out := append([]Candidate(nil), cs...)
	sort.SliceStable(out, func(i, j int) bool { return score(out[i]) < score(out[j]) })
	return out
}

func score(c Candidate) float64 {
	if c.Pace == nil {
		return 1000 // unknown: after reserve, before deficit
	}
	if !c.Pace.WillLast {
		return 3000 + c.Pace.Delta
	}
	if c.Pace.Deficit {
		return 2000 + c.Pace.Delta
	}
	return c.Pace.Delta // negative = reserve, more negative first
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
