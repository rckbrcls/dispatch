package stats

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"dispatch/internal/pace"
	"dispatch/internal/store"
)

const (
	CapacitySchema = "dispatch.capacity.v1"
	fullPercent    = 99.5
)

// CapacityReport is usage of each quota pool against its own window.
// Agents with no snapshots are absent: their capacity is unknown.
type CapacityReport struct {
	Schema string        `json:"schema"`
	Since  string        `json:"since,omitempty"`
	Note   string        `json:"note"`
	Keys   []CapacityKey `json:"keys"`
}

// CapacityKey rolls up one snapshot key (one CLI subscription pool).
type CapacityKey struct {
	Key             string       `json:"key"`
	Windows         int          `json:"windows"`
	HitFull         int          `json:"hitFull"`
	ConsumedPct     *float64     `json:"consumedPct,omitempty"`
	LeftoverPct     *float64     `json:"leftoverPct,omitempty"`
	Leftover        string       `json:"leftover,omitempty"` // "desconhecido" when none of the windows closed observably
	LeftoverKnown   int          `json:"leftoverKnown"`
	LeftoverUnknown int          `json:"leftoverUnknown"`
	Pace            string       `json:"pace"`
	Detail          []WindowStat `json:"windowsDetail"`
}

// WindowStat is one quota window. An open window has no leftover yet.
type WindowStat struct {
	ResetsAt    string   `json:"resetsAt"`
	Open        bool     `json:"open"`
	ConsumedPct float64  `json:"consumedPct"`
	LeftoverPct *float64 `json:"leftoverPct,omitempty"`
	Leftover    string   `json:"leftover,omitempty"`
	HitFull     bool     `json:"hitFull"`
	Pace        string   `json:"pace"`
}

type winBucket struct {
	key    string
	reset  time.Time
	window time.Duration
	snaps  []pace.Snapshot
}

// CapacityFromStore reads snapshots.jsonl only.
func CapacityFromStore(s store.Store, since, now time.Time) (CapacityReport, error) {
	snaps, err := s.AllSnapshots()
	if err != nil {
		return CapacityReport{}, err
	}
	return Capacity(snaps, since, now), nil
}

// Capacity summarizes snapshots. Percentages are of each pool's own window.
func Capacity(snaps []store.SnapshotRecord, since, now time.Time) CapacityReport {
	if now.IsZero() {
		now = time.Now()
	}
	rep := CapacityReport{
		Schema: CapacitySchema,
		Note:   "agentes sem snapshots têm capacidade desconhecida; percentual é da janela de cada um, não de tokens",
		Keys:   []CapacityKey{},
	}
	if !since.IsZero() {
		rep.Since = since.UTC().Format(time.RFC3339)
	}
	if len(snaps) == 0 {
		rep.Note = "desconhecido: sem snapshots; " + rep.Note
		return rep
	}
	groups := map[string]*winBucket{}
	var order []string
	for _, s := range snaps {
		if s.Key == "" {
			continue
		}
		reset := s.ResetsAt.UTC().Truncate(time.Hour)
		id := s.Key + "|" + reset.Format(time.RFC3339)
		b := groups[id]
		if b == nil {
			b = &winBucket{key: s.Key, reset: reset}
			groups[id] = b
			order = append(order, id)
		}
		if s.WindowMinutes > 0 {
			b.window = time.Duration(s.WindowMinutes) * time.Minute
		}
		b.snaps = append(b.snaps, s.Snapshot)
	}
	byKey := map[string][]WindowStat{}
	var latestAt = map[string]time.Time{}
	var latestPace = map[string]string{}
	for _, id := range order {
		b := groups[id]
		if !windowInSince(*b, since) {
			continue
		}
		st := windowStat(*b, now)
		byKey[b.key] = append(byKey[b.key], st)
		last := latestSample(b.snaps)
		if last.SampledAt.After(latestAt[b.key]) || latestAt[b.key].IsZero() {
			latestAt[b.key] = last.SampledAt
			latestPace[b.key] = st.Pace
		}
	}
	var keys []string
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ws := byKey[k]
		sort.Slice(ws, func(i, j int) bool { return ws[i].ResetsAt < ws[j].ResetsAt })
		ck := CapacityKey{
			Key: k, Windows: len(ws), Detail: ws,
			Pace: latestPace[k],
		}
		if ck.Pace == "" {
			ck.Pace = "desconhecido"
		}
		var consumed, leftover []float64
		for _, w := range ws {
			consumed = append(consumed, w.ConsumedPct)
			if w.HitFull {
				ck.HitFull++
			}
			if w.LeftoverPct != nil {
				leftover = append(leftover, *w.LeftoverPct)
				ck.LeftoverKnown++
			} else {
				ck.LeftoverUnknown++
			}
		}
		if len(consumed) > 0 {
			v := round1(mean(consumed))
			ck.ConsumedPct = &v
		}
		if len(leftover) > 0 {
			v := round1(mean(leftover))
			ck.LeftoverPct = &v
		} else {
			ck.Leftover = "desconhecido"
		}
		rep.Keys = append(rep.Keys, ck)
	}
	return rep
}

func windowInSince(b winBucket, since time.Time) bool {
	if since.IsZero() {
		return true
	}
	if !b.reset.Before(since) {
		return true
	}
	for _, s := range b.snaps {
		if !s.SampledAt.Before(since) {
			return true
		}
	}
	return false
}

func windowStat(b winBucket, now time.Time) WindowStat {
	last := latestSample(b.snaps)
	peak := b.snaps[0].UsedPercent
	for _, s := range b.snaps[1:] {
		if s.UsedPercent > peak {
			peak = s.UsedPercent
		}
	}
	reset := last.ResetsAt
	if reset.IsZero() {
		reset = b.reset
	}
	st := WindowStat{
		ResetsAt:    b.reset.UTC().Format(time.RFC3339),
		Open:        reset.After(now),
		ConsumedPct: round1(last.UsedPercent),
		HitFull:     peak >= fullPercent,
		Pace:        paceLabel(last.UsedPercent, b.window, reset, last.SampledAt),
	}
	if left, ok := leftoverAtReset(reset, b.window, last, now); ok {
		v := round1(left)
		st.LeftoverPct = &v
	} else {
		st.Leftover = "desconhecido"
	}
	return st
}

func leftoverAtReset(reset time.Time, window time.Duration, last pace.Snapshot, now time.Time) (float64, bool) {
	if reset.After(now) || last.SampledAt.IsZero() {
		return 0, false
	}
	if window > 0 && reset.Sub(last.SampledAt) > window/4 {
		return 0, false
	}
	left := 100 - last.UsedPercent
	if left < 0 {
		left = 0
	}
	return left, true
}

func latestSample(snaps []pace.Snapshot) pace.Snapshot {
	best := snaps[0]
	for _, s := range snaps[1:] {
		if s.SampledAt.After(best.SampledAt) {
			best = s
		}
	}
	return best
}

func paceLabel(used float64, window time.Duration, resetsAt, at time.Time) string {
	if window <= 0 || at.IsZero() || resetsAt.IsZero() {
		return "desconhecido"
	}
	p := pace.Linear(used, window, resetsAt, at)
	switch {
	case p.Delta > 2:
		return "adiantado"
	case p.Delta < -2:
		return "atrasado"
	default:
		return "no ritmo"
	}
}

func mean(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func round1(f float64) float64 {
	return math.Round(f*10) / 10
}

// Text renders one block per quota pool.
func (r CapacityReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "capacity  %s\n", r.Note)
	if len(r.Keys) == 0 {
		b.WriteString("sem janelas\n")
		return b.String()
	}
	for _, k := range r.Keys {
		left := "desconhecido"
		if k.LeftoverPct != nil {
			left = fmt.Sprintf("%.1f%%", *k.LeftoverPct)
		}
		cons := "desconhecido"
		if k.ConsumedPct != nil {
			cons = fmt.Sprintf("%.1f%%", *k.ConsumedPct)
		}
		fmt.Fprintf(&b, "%s  windows=%d  hit100=%d  consumed=%s  leftover=%s  unknownLeftover=%d  pace=%s\n",
			k.Key, k.Windows, k.HitFull, cons, left, k.LeftoverUnknown, k.Pace)
		for _, w := range k.Detail {
			wl := "desconhecido"
			if w.LeftoverPct != nil {
				wl = fmt.Sprintf("%.1f%%", *w.LeftoverPct)
			}
			fmt.Fprintf(&b, "  reset %s  consumed=%.1f%%  leftover=%s  hit100=%t  pace=%s  open=%t\n",
				w.ResetsAt, w.ConsumedPct, wl, w.HitFull, w.Pace, w.Open)
		}
	}
	return b.String()
}
