package pace

import (
	"math"
	"sort"
	"time"
)

const (
	week        = 7 * 24 * time.Hour
	gridPoints  = 169 // hourly over a week, inclusive of both ends
	minSamples  = 6
	minWeeks    = 5
	ageHalfLife = 3.0 // weeks, as the exp(-age/3) decay constant
)

// Snapshot is one observed point of a weekly quota window.
type Snapshot struct {
	UsedPercent   float64   `json:"usedPercent"`
	ResetsAt      time.Time `json:"resetsAt"`
	WindowMinutes int       `json:"windowMinutes"`
	SampledAt     time.Time `json:"sampledAt"`
}

// History is the pace estimate derived from past weeks.
type History struct {
	Valid bool
	Weeks int // complete past weeks used
	// Risk is the smoothed, weighted fraction of past weeks that, shifted to
	// current usage, would hit 100% before the reset.
	Risk float64
	// RunsOutIn is the weighted median time until exhaustion across those
	// weeks (zero when no week crossed).
	RunsOutIn time.Duration
	// Expected is the blended expected-used% at the current point of the week.
	Expected float64
}

type weekCurve struct {
	curve [gridPoints]float64
	w     float64
}

// Historical estimates risk for a weekly window from past snapshots.
// resetsAt/usedPercent describe the current week. It returns Valid=false
// when fewer than five complete weeks exist.
func Historical(snaps []Snapshot, usedPercent float64, resetsAt, now time.Time) History {
	curves := buildCurves(snaps, resetsAt, now)
	h := History{Weeks: len(curves)}
	if len(curves) < minWeeks {
		return h
	}
	start := resetsAt.Add(-week)
	elapsed := now.Sub(start)
	if elapsed < 0 {
		elapsed = 0
	}
	idx := int(math.Round(elapsed.Hours()))
	if idx >= gridPoints {
		idx = gridPoints - 1
	}

	var sw, sw2 float64
	for _, c := range curves {
		sw += c.w
		sw2 += c.w * c.w
	}
	nEff := sw * sw / sw2
	lambda := clamp((nEff-2)/6, 0, 1)
	linear := float64(idx) / float64(gridPoints-1) * 100
	med := weightedMedian(valuesAt(curves, idx), weightsOf(curves))
	h.Expected = math.Min(lambda*med+(1-lambda)*linear, linear)

	var crossMass float64
	var crossHours, crossW []float64
	for _, c := range curves {
		shift := usedPercent - c.curve[idx]
		for j := idx; j < gridPoints; j++ {
			if c.curve[j]+shift >= 100 {
				crossMass += c.w
				crossHours = append(crossHours, float64(j-idx))
				crossW = append(crossW, c.w)
				break
			}
		}
	}
	h.Valid = true
	h.Risk = (crossMass + 0.5) / (sw + 1)
	if len(crossHours) > 0 {
		h.RunsOutIn = time.Duration(weightedMedian(crossHours, crossW) * float64(time.Hour))
	}
	return h
}

// buildCurves reconstructs a monotone hourly curve for each complete past
// week: >=6 samples with coverage in the first and last 24 hours.
func buildCurves(snaps []Snapshot, currentReset, now time.Time) []weekCurve {
	byWeek := map[int64][]Snapshot{}
	for _, s := range snaps {
		if s.WindowMinutes != int(week/time.Minute) {
			continue
		}
		key := s.ResetsAt.Round(time.Hour).Unix()
		byWeek[key] = append(byWeek[key], s)
	}
	curKey := currentReset.Round(time.Hour).Unix()
	var out []weekCurve
	for key, ss := range byWeek {
		if key == curKey {
			continue
		}
		end := time.Unix(key, 0)
		if end.After(now) || len(ss) < minSamples {
			continue
		}
		start := end.Add(-week)
		sort.Slice(ss, func(i, j int) bool { return ss[i].SampledAt.Before(ss[j].SampledAt) })
		first, last := ss[0].SampledAt, ss[len(ss)-1].SampledAt
		if first.After(start.Add(24*time.Hour)) || last.Before(end.Add(-24*time.Hour)) {
			continue
		}
		var c weekCurve
		si, cur, peak := 0, 0.0, 0.0
		for g := 0; g < gridPoints; g++ {
			t := start.Add(time.Duration(g) * time.Hour)
			for si < len(ss) && !ss[si].SampledAt.After(t) {
				cur = ss[si].UsedPercent
				si++
			}
			if g == 0 && si == 0 {
				cur = ss[0].UsedPercent
			}
			peak = math.Max(peak, cur)
			c.curve[g] = peak
		}
		age := now.Sub(end).Hours() / (7 * 24)
		c.w = math.Exp(-age / ageHalfLife)
		out = append(out, c)
	}
	return out
}

func valuesAt(cs []weekCurve, idx int) []float64 {
	v := make([]float64, len(cs))
	for i, c := range cs {
		v[i] = c.curve[idx]
	}
	return v
}

func weightsOf(cs []weekCurve) []float64 {
	w := make([]float64, len(cs))
	for i, c := range cs {
		w[i] = c.w
	}
	return w
}

func weightedMedian(vals, weights []float64) float64 {
	type vw struct{ v, w float64 }
	xs := make([]vw, len(vals))
	var total float64
	for i := range vals {
		xs[i] = vw{vals[i], weights[i]}
		total += weights[i]
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i].v < xs[j].v })
	var acc float64
	for _, x := range xs {
		acc += x.w
		if acc >= total/2 {
			return x.v
		}
	}
	return 0
}

func clamp(x, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, x)) }
