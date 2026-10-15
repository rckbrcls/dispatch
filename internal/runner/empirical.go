package runner

import (
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/store"
)

const empiricalWindow = 24 * time.Hour

// EmpiricalQuota is the risk sample for an agent with no official quota.
// Used is always nil.
func EmpiricalQuota(st store.Store, agent string, now time.Time) adapter.QuotaSample {
	if now.IsZero() {
		now = time.Now()
	}
	until := st.Cooldowns(now)[agent]
	n := 0
	recs, err := st.List()
	if err == nil {
		cutoff := now.Add(-empiricalWindow)
		for _, r := range recs {
			if !r.FinishedAt.After(cutoff) && !r.CreatedAt.After(cutoff) {
				continue
			}
			if r.Agent == agent && rateLimited.MatchString(r.Error) {
				n++
			}
			for _, at := range r.Attempts {
				if at.Agent == agent && (at.ExitClass == "rate_limit" || rateLimited.MatchString(at.Error)) {
					n++
				}
			}
		}
	}
	return adapter.EmpiricalSample(now, until, n)
}
