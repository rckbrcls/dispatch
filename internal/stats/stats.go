// Package stats aggregates runs.jsonl and snapshots.jsonl. It does not
// estimate tokens or cost and it does not talk to agents.
package stats

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"dispatch/internal/store"
)

const (
	Schema     = "dispatch.stats.v1"
	MinSamples = 5
)

// rateRe is the runner's rate-limit detector so older rows, which have
// no exitClass, still count.
var rateRe = store.RateLimitRe

// Options selects the aggregation. A zero Since includes every run.
type Options struct {
	By    string
	Since time.Time
	Now   time.Time
}

// Report is the dispatch.stats.v1 document.
type Report struct {
	Schema     string   `json:"schema"`
	Since      string   `json:"since,omitempty"`
	By         string   `json:"by"`
	MinSamples int      `json:"minSamples"`
	InFlight   int      `json:"inFlight,omitempty"`
	Rows       []Row    `json:"rows"`
	Rankings   Rankings `json:"rankings"`
	Note       string   `json:"note,omitempty"`
}

// Row is one group. A nil percentile means the duration is unknown.
type Row struct {
	Key         string  `json:"key"`
	Runs        int     `json:"runs"`
	SuccessPct  float64 `json:"successPct"`
	FirstTryPct float64 `json:"firstTryPct"`
	P50Ms       *int64  `json:"p50Ms,omitempty"`
	P90Ms       *int64  `json:"p90Ms,omitempty"`
	Duration    string  `json:"duration,omitempty"` // "desconhecido" when no samples
	DurationN   int     `json:"durationSamples"`
	Fallbacks   int     `json:"fallbacks"`
	RateLimits  int     `json:"rateLimits"`
	Orphans     int     `json:"orphans"`
}

// Rankings compare models and providers. A non-empty Reason means the rank
// was withheld; Key is then "desconhecido".
type Rankings struct {
	MostUsedModel    Rank `json:"mostUsedModel"`
	MostUsedProvider Rank `json:"mostUsedProvider"`
	SlowestModel     Rank `json:"slowestModel"`
	MostFailedModel  Rank `json:"mostFailedModel"`
}

// Rank is one ranking. Comparative ranks require MinSamples.
type Rank struct {
	Key        string   `json:"key"`
	Runs       int      `json:"runs,omitempty"`
	Samples    int      `json:"samples,omitempty"`
	P50Ms      *int64   `json:"p50Ms,omitempty"`
	SuccessPct *float64 `json:"successPct,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// ValidBy reports whether --by accepts name.
func ValidBy(name string) bool {
	switch name {
	case "model", "agent", "provider", "tier", "effort", "day", "week", "requested", "resolved":
		return true
	default:
		return false
	}
}

// ParseSince parses 30m, 12h, 7d, 2w, or all. The time is the inclusive start.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "all" || s == "0" {
		return time.Time{}, nil
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i == len(s) {
		return time.Time{}, fmt.Errorf("bad --since %q (want 7d, 24h, 30m, 2w, all)", s)
	}
	n := 0
	for _, c := range s[:i] {
		n = n*10 + int(c-'0')
	}
	var unit time.Duration
	switch s[i:] {
	case "m":
		unit = time.Minute
	case "h":
		unit = time.Hour
	case "d":
		unit = 24 * time.Hour
	case "w":
		unit = 7 * 24 * time.Hour
	default:
		return time.Time{}, fmt.Errorf("bad --since %q (want 7d, 24h, 30m, 2w, all)", s)
	}
	return now.Add(-time.Duration(n) * unit), nil
}

// FromStore aggregates the latest record of every run, including rotated logs.
func FromStore(s store.Store, opt Options) (Report, error) {
	recs, err := s.List()
	if err != nil {
		return Report{}, err
	}
	return Compute(recs, opt)
}

// Compute aggregates recs. Pending and running runs are counted as inFlight
// and left out of the rates.
func Compute(recs []store.Record, opt Options) (Report, error) {
	if opt.By == "" {
		opt.By = "model"
	}
	if !ValidBy(opt.By) {
		return Report{}, fmt.Errorf("unknown --by %q", opt.By)
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	rep := Report{
		Schema: Schema, By: opt.By, MinSamples: MinSamples,
		Rows: []Row{},
		Note: "campo ausente é desconhecido; duração usa runMs ou, se faltar, finishedAt-startedAt/createdAt",
	}
	if !opt.Since.IsZero() {
		rep.Since = opt.Since.UTC().Format(time.RFC3339)
	}
	var kept []store.Record
	for _, r := range recs {
		if !opt.Since.IsZero() && (r.CreatedAt.IsZero() || r.CreatedAt.Before(opt.Since)) {
			continue
		}
		if r.Status == store.Pending || r.Status == store.Running {
			rep.InFlight++
			continue
		}
		kept = append(kept, r)
	}
	rep.Rows = rowsOf(buckets(kept, opt.By), opt.By)
	rep.Rankings = rankAll(kept)
	return rep, nil
}

type acc struct {
	runs, ok, first, fallbacks, rates, orphans int
	durs                                       []int64
}

func buckets(recs []store.Record, by string) map[string]*acc {
	out := map[string]*acc{}
	for _, r := range recs {
		k := groupKey(r, by)
		a := out[k]
		if a == nil {
			a = &acc{}
			out[k] = a
		}
		a.runs++
		if r.Status == store.Completed {
			a.ok++
			if len(r.Attempts) == 0 {
				a.first++
			}
		}
		if len(r.Attempts) > 0 {
			a.fallbacks++
		}
		if hitRateLimit(r) {
			a.rates++
		}
		if r.Status == store.Orphaned {
			a.orphans++
		}
		if ms, ok := r.ObservedMs(); ok {
			a.durs = append(a.durs, ms)
		}
	}
	return out
}

func groupKey(r store.Record, by string) string {
	switch by {
	case "requested", "resolved":
		f := r.Requested
		if by == "resolved" {
			f = r.Resolved
		}
		if f == nil {
			return "desconhecido"
		}
		b, _ := json.Marshal(f)
		return string(b)
	case "model":
		return known(r.Model)
	case "agent":
		return known(r.Agent)
	case "provider":
		return known(r.Provider)
	case "tier":
		return known(r.Tier)
	case "effort":
		return known(r.Effort)
	case "day":
		if r.CreatedAt.IsZero() {
			return "desconhecido"
		}
		return r.CreatedAt.UTC().Format("2006-01-02")
	case "week":
		if r.CreatedAt.IsZero() {
			return "desconhecido"
		}
		y, w := r.CreatedAt.UTC().ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	default:
		return "desconhecido"
	}
}

func known(s string) string {
	if s == "" {
		return "desconhecido"
	}
	return s
}

func rowsOf(groups map[string]*acc, by string) []Row {
	rows := make([]Row, 0, len(groups))
	for k, a := range groups {
		row := Row{
			Key: k, Runs: a.runs,
			SuccessPct:  ratio(a.ok, a.runs),
			FirstTryPct: ratio(a.first, a.runs),
			DurationN:   len(a.durs),
			Fallbacks:   a.fallbacks,
			RateLimits:  a.rates,
			Orphans:     a.orphans,
		}
		if p, ok := pctile(a.durs, 0.50); ok {
			row.P50Ms = store.I64(p)
		}
		if p, ok := pctile(a.durs, 0.90); ok {
			row.P90Ms = store.I64(p)
		}
		if row.DurationN == 0 {
			row.Duration = "desconhecido"
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if by == "day" || by == "week" {
			return rows[i].Key < rows[j].Key
		}
		if rows[i].Runs != rows[j].Runs {
			return rows[i].Runs > rows[j].Runs
		}
		return rows[i].Key < rows[j].Key
	})
	return rows
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return math.Round(float64(n)/float64(d)*1000) / 10
}

func pctile(xs []int64, p float64) (int64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(math.Ceil(p*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx], true
}

func hitRateLimit(r store.Record) bool {
	if r.ExitClass == "rate_limit" || rateRe.MatchString(r.Error) {
		return true
	}
	for _, a := range r.Attempts {
		if a.ExitClass == "rate_limit" || rateRe.MatchString(a.Error) {
			return true
		}
	}
	return false
}

func rankAll(recs []store.Record) Rankings {
	return Rankings{
		MostUsedModel:    mostUsed(buckets(recs, "model")),
		MostUsedProvider: mostUsed(buckets(recs, "provider")),
		SlowestModel:     slowest(buckets(recs, "model")),
		MostFailedModel:  mostFailed(buckets(recs, "model")),
	}
}

func withheld(reason string) Rank {
	return Rank{Key: "desconhecido", Reason: reason}
}

func mostUsed(groups map[string]*acc) Rank {
	if len(groups) == 0 {
		return withheld("sem runs no período")
	}
	best := ""
	bestN := -1
	for k, a := range groups {
		if a.runs > bestN || (a.runs == bestN && (best == "" || k < best)) {
			best, bestN = k, a.runs
		}
	}
	return Rank{Key: best, Runs: bestN}
}

func slowest(groups map[string]*acc) Rank {
	var best *Rank
	var bestP90 int64
	for k, a := range groups {
		if len(a.durs) < MinSamples {
			continue
		}
		p50, _ := pctile(a.durs, 0.50)
		p90, _ := pctile(a.durs, 0.90)
		cand := Rank{Key: k, Runs: a.runs, Samples: len(a.durs), P50Ms: store.I64(p50)}
		if best == nil || p50 > *best.P50Ms || (p50 == *best.P50Ms && (p90 > bestP90 || (p90 == bestP90 && k < best.Key))) {
			c := cand
			best = &c
			bestP90 = p90
		}
	}
	if best == nil {
		return withheld(fmt.Sprintf("amostras insuficientes (mínimo %d)", MinSamples))
	}
	return *best
}

func mostFailed(groups map[string]*acc) Rank {
	bestK := ""
	var best *acc
	for k, a := range groups {
		if a.runs < MinSamples {
			continue
		}
		if best == nil || worse(a, best, k, bestK) {
			best, bestK = a, k
		}
	}
	if best == nil {
		return withheld(fmt.Sprintf("amostras insuficientes (mínimo %d)", MinSamples))
	}
	p := ratio(best.ok, best.runs)
	return Rank{Key: bestK, Runs: best.runs, SuccessPct: &p}
}

func worse(a, b *acc, ak, bk string) bool {
	ap, bp := ratio(a.ok, a.runs), ratio(b.ok, b.runs)
	if ap != bp {
		return ap < bp
	}
	if a.runs != b.runs {
		return a.runs > b.runs
	}
	return ak < bk
}

// Text renders the table and the rankings. Unknown durations say "desconhecido".
func (r Report) Text() string {
	var b strings.Builder
	since := r.Since
	if since == "" {
		since = "all"
	}
	fmt.Fprintf(&b, "since %s  by %s  minSamples %d\n", since, r.By, r.MinSamples)
	if r.InFlight > 0 {
		fmt.Fprintf(&b, "inFlight %d (fora das taxas)\n", r.InFlight)
	}
	if len(r.Rows) == 0 {
		b.WriteString("sem runs no período\n")
	} else {
		fmt.Fprintf(&b, "%-24s %5s %6s %6s %12s %12s %9s %10s %7s\n",
			r.By, "runs", "ok%", "first%", "p50", "p90", "fallbacks", "rateLimits", "orphans")
		for _, row := range r.Rows {
			fmt.Fprintf(&b, "%-24s %5d %6.1f %6.1f %12s %12s %9d %10d %7d\n",
				trunc(row.Key, 24), row.Runs, row.SuccessPct, row.FirstTryPct,
				fmtDur(row.P50Ms), fmtDur(row.P90Ms), row.Fallbacks, row.RateLimits, row.Orphans)
		}
	}
	b.WriteString(r.Rankings.MostUsedModel.line("most used model"))
	b.WriteString(r.Rankings.MostUsedProvider.line("most used provider"))
	b.WriteString(r.Rankings.SlowestModel.line("slowest model"))
	b.WriteString(r.Rankings.MostFailedModel.line("most failed model"))
	return b.String()
}

func (r Rank) line(label string) string {
	if r.Reason != "" || r.Key == "" {
		reason := r.Reason
		if reason == "" {
			reason = "desconhecido"
		}
		return fmt.Sprintf("%s: desconhecido (%s)\n", label, reason)
	}
	switch {
	case r.P50Ms != nil:
		return fmt.Sprintf("%s: %s (p50 %s, %d samples)\n", label, r.Key, fmtDur(r.P50Ms), r.Samples)
	case r.SuccessPct != nil:
		return fmt.Sprintf("%s: %s (%.1f%% ok, %d runs)\n", label, r.Key, *r.SuccessPct, r.Runs)
	default:
		return fmt.Sprintf("%s: %s (%d runs)\n", label, r.Key, r.Runs)
	}
}

func fmtDur(ms *int64) string {
	if ms == nil {
		return "desconhecido"
	}
	if *ms < 1000 {
		return fmt.Sprintf("%dms", *ms)
	}
	d := time.Duration(*ms) * time.Millisecond
	return d.Round(100 * time.Millisecond).String()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
