package tui

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"dispatch/internal/adapter"
	"dispatch/internal/classify"
	"dispatch/internal/stats"
	"dispatch/internal/store"
)

var learnTiers = []adapter.Tier{adapter.TierEasy, adapter.TierNormal, adapter.TierHard}

func (m *model) learnKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "l", "esc", "left", "h":
		m.learnOn = false
		m.render()
		m.reveal()
		return m, nil
	case "s":
		m.learnOn, m.statsOn = false, true
		m.renderStats()
		m.vp.GotoTop()
		return m, nil
	case "a":
		if m.root != "" {
			m.showAll = !m.showAll
			m.refresh(true)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// scopedAll is every run in the current repository scope, any age.
func (m *model) scopedAll() []store.Record {
	var out []store.Record
	for _, r := range m.all {
		if m.match(r) {
			out = append(out, r)
		}
	}
	return out
}

func (m *model) renderLearn() {
	if !m.ready {
		return
	}
	w := max(m.w-2, 40)
	recs := m.scopedAll()
	var b strings.Builder
	out := func(s string) { b.WriteString(s + "\n") }
	sec := func(title, note string) {
		out("")
		out(heading(title, w))
		if note != "" {
			for _, l := range strings.Split(ansi.Wrap(note, w-2, " "), "\n") {
				out(dim.Render("  " + l))
			}
		}
		out("")
	}

	model, _ := classify.Load(filepath.Join(filepath.Dir(m.opts.Store.Path), "classifier.json"))
	prior := classify.Prior()
	names := classify.FeatureNames()

	// per-tier labelled outcomes under the current model
	type tierAcc struct {
		eligible, failed, warned, warnedFailed, promoted int
		scoreSum                                         float64
	}
	acc := map[adapter.Tier]*tierAcc{}
	for _, t := range learnTiers {
		acc[t] = &tierAcc{}
	}
	type bin struct {
		n, failed int
		sum       float64
	}
	bins := make([]bin, 4)
	binOf := func(p float64) int { return min(int(p/0.2), 3) }
	for _, r := range recs {
		t := adapter.Tier(coalesce(r.ExecutionTier, r.Tier))
		a, ok := acc[t]
		if !ok {
			continue
		}
		warn := strings.Contains(r.Note, "classifier warning")
		if strings.Contains(r.Note, "sensitive code: tier") {
			a.promoted++
		}
		failed, eligible := classify.Label(r)
		if warn && eligible {
			a.warned++
			if failed {
				a.warnedFailed++
			}
		}
		if !eligible || r.FeatureSchema != classify.FeatureSchema || len(r.ClassifyFeatures) != len(names) {
			continue
		}
		var f classify.Features
		copy(f[:], r.ClassifyFeatures)
		score := model.Predict(t, f, false).Score
		a.eligible++
		a.scoreSum += score
		i := binOf(score)
		bins[i].n++
		bins[i].sum += score
		if failed {
			a.failed++
			bins[i].failed++
		}
	}

	// 1. classifier
	sec("Classifier", fmt.Sprintf("Predicts the chance a run fails verification, per tier. It starts learning after %d labelled runs in a tier.", classify.MinSamples))
	var rows [][]string
	for _, t := range learnTiers {
		h := model.Tiers[t]
		a := acc[t]
		phase := dim.Render(fmt.Sprintf("warming up %d/%d", h.Samples, classify.MinSamples))
		if h.Samples >= classify.MinSamples {
			phase = lipgloss.NewStyle().Foreground(cOK).Render("learning")
		}
		fail, avg := "—", "—"
		if a.eligible > 0 {
			fail = pct(100 * float64(a.failed) / float64(a.eligible))
			avg = pct(100 * a.scoreSum / float64(a.eligible))
		}
		rows = append(rows, []string{text.Bold(true).Render(string(t)), fmt.Sprint(h.Samples), phase, fmt.Sprint(a.eligible), fail, avg,
			fmt.Sprintf("%d / %d", a.warnedFailed, a.warned), fmt.Sprint(a.promoted)})
	}
	out(grid([]string{"tier", "samples", "phase", "labelled", "failed", "predicted", "warned/failed", "promoted"}, rows, 1, 3, 4, 5, 6, 7))
	for _, l := range strings.Split(ansi.Wrap("failed = real verification failures · predicted = average score the model gives those same runs · warned = runs it flagged · promoted = raised a tier for sensitive code", w-2, " "), "\n") {
		out(dim.Render("  " + l))
	}

	// 2. what it learned
	sec("What it learned", "Weights that moved furthest from the starting prior. Positive raises the failure risk, negative lowers it.")
	learned := false
	for _, t := range learnTiers {
		h := model.Tiers[t]
		if h.Samples < classify.MinSamples {
			continue
		}
		learned = true
		type mv struct {
			name     string
			w, delta float64
		}
		var mvs []mv
		for i, n := range names {
			if n == "bias" || strings.HasPrefix(n, "tier_") {
				continue
			}
			d := h.Weights[i] - prior.Tiers[t].Weights[i]
			if math.Abs(d) > 0.005 {
				mvs = append(mvs, mv{n, h.Weights[i], d})
			}
		}
		sort.Slice(mvs, func(i, j int) bool { return math.Abs(mvs[i].delta) > math.Abs(mvs[j].delta) })
		if len(mvs) > 5 {
			mvs = mvs[:5]
		}
		var wr [][]string
		for _, v := range mvs {
			c := cBad
			dir := "raises risk"
			if v.delta < 0 {
				c, dir = cOK, "lowers risk"
			}
			wr = append(wr, []string{v.name, fmt.Sprintf("%+.2f", v.w), lipgloss.NewStyle().Foreground(c).Render(fmt.Sprintf("%+.2f", v.delta)),
				bar(math.Min(math.Abs(v.delta)/2, 1), 10, c), dim.Render(dir)})
		}
		out("  " + accent.Bold(true).Render(string(t)) + dim.Render(fmt.Sprintf("  %d samples", h.Samples)))
		if len(wr) == 0 {
			out(dim.Render("    no weight has moved yet"))
		} else {
			out(grid([]string{"feature", "weight", "change", "", ""}, wr, 1, 2))
		}
		out("")
	}
	if !learned {
		out(dim.Render("  Nothing yet: every tier is still on its starting prior."))
	}

	// 3. calibration
	sec("Calibration", "Predicted failure risk vs what actually happened (measured on past runs with the current model, so optimistic).")
	var cr [][]string
	labels := []string{"0–20%", "20–40%", "40–60%", "60–100%"}
	any := false
	for i, bn := range bins {
		real, pred := "—", "—"
		if bn.n > 0 {
			any = true
			rf := 100 * float64(bn.failed) / float64(bn.n)
			real = lipgloss.NewStyle().Foreground(pctColor(100 - rf)).Render(pct(rf))
			pred = pct(100 * bn.sum / float64(bn.n))
		}
		cr = append(cr, []string{labels[i], fmt.Sprint(bn.n), pred, real})
	}
	if any {
		out(grid([]string{"predicted risk", "runs", "avg predicted", "actually failed"}, cr, 1, 2, 3))
	} else {
		out(dim.Render("  No labelled runs with classifier features yet. Runs need a verification verdict (not read-only, not --no-verify)."))
	}

	// 4. learning curve
	sec("Learning curve", "Outcome quality by week. If routing is learning, first-try and verified rates should hold or rise while time falls.")
	if rep, err := stats.Compute(recs, stats.Options{By: "week", Now: m.now}); err == nil && len(rep.Rows) > 0 {
		rws := rep.Rows
		sort.Slice(rws, func(i, j int) bool { return rws[i].Key < rws[j].Key })
		if len(rws) > 12 {
			rws = rws[len(rws)-12:]
		}
		var wr [][]string
		prev := -1.0
		for _, r := range rws {
			trend := dim.Render("·")
			if prev >= 0 {
				switch d := r.FirstTryPct - prev; {
				case d > 1:
					trend = lipgloss.NewStyle().Foreground(cOK).Render(fmt.Sprintf("▲ %+.0f", d))
				case d < -1:
					trend = lipgloss.NewStyle().Foreground(cBad).Render(fmt.Sprintf("▼ %+.0f", d))
				}
			}
			prev = r.FirstTryPct
			key := r.Key
			if key == "desconhecido" {
				key = "unknown"
			}
			wr = append(wr, []string{text.Bold(true).Render(key), fmt.Sprint(r.Runs),
				bar(r.FirstTryPct/100, 10, pctColor(r.FirstTryPct)) + " " + pct(r.FirstTryPct), pct(r.SuccessPct), trend,
				msp(r.P50Ms), fmt.Sprint(r.Fallbacks), fmt.Sprint(r.RateLimits)})
		}
		out(grid([]string{"week", "runs", "first try", "success", "vs prev", "p50", "fallbacks", "rate limits"}, wr, 1, 3, 4, 5, 6, 7))
	} else {
		out(dim.Render("  Not enough history yet."))
	}

	// 5. model evidence
	sec("Model evidence", fmt.Sprintf("What routing knows about each model per tier. From %d samples it blends this evidence with the built-in speed and strength priors.", stats.MinSamples))
	ev := stats.EvidenceByTierModel(recs)
	var evs []stats.ModelEvidence
	for _, e := range ev {
		evs = append(evs, e)
	}
	order := map[string]int{"easy": 0, "normal": 1, "hard": 2}
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].Tier != evs[j].Tier {
			return order[evs[i].Tier] < order[evs[j].Tier]
		}
		return evs[i].Samples > evs[j].Samples
	})
	if len(evs) == 0 {
		out(dim.Render("  No finished runs yet."))
	} else {
		var er [][]string
		for _, e := range evs {
			status := dim.Render("collecting")
			if e.Samples >= stats.MinSamples {
				status = lipgloss.NewStyle().Foreground(cOK).Render("in routing")
			}
			vr := "—"
			if e.Verified > 0 || e.Samples >= stats.MinSamples {
				vr = lipgloss.NewStyle().Foreground(pctColor(100 * e.VerifiedRate)).Render(pct(100 * e.VerifiedRate))
			}
			er = append(er, []string{text.Bold(true).Render(e.Tier), ansi.Truncate(e.Model, 28, "…"), fmt.Sprint(e.Samples),
				bar(math.Min(float64(e.Samples)/stats.MinSamples, 1), 8, cAccent), status, vr, msp(e.P50Ms)})
		}
		out(grid([]string{"tier", "model", "samples", "progress", "status", "verified", "p50"}, er, 2, 5, 6))
	}

	// 6. cooldowns
	sec("Rate-limit memory", "Agents the router is avoiding after a rate limit. Expires on its own.")
	cd := m.opts.Store.Cooldowns(m.now)
	if len(cd) == 0 {
		out(dim.Render("  None. Every agent is available."))
	} else {
		var names []string
		for k := range cd {
			names = append(names, k)
		}
		sort.Strings(names)
		var cr [][]string
		for _, k := range names {
			cr = append(cr, []string{text.Bold(true).Render(k), fmtDur(cd[k].Sub(m.now).Truncate(time.Second)), cd[k].Local().Format("15:04")})
		}
		out(grid([]string{"agent", "back in", "at"}, cr, 1))
	}

	m.vp.SetContent(strings.TrimRight(b.String(), "\n"))
}

func (m *model) learnView() string {
	scope := "all repositories"
	if m.root != "" && !m.showAll {
		scope = filepath.Base(m.root)
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(cAccent).Render("▍dispatch") + dim.Render("  learning · "+scope)
	scopeKey := "all repos"
	if m.showAll {
		scopeKey = "this repo"
	}
	help := dim.Render(fit(m.w,
		"↑↓ pgup/pgdn scroll · a "+scopeKey+" · s stats · l/esc back · q quit",
		"↑↓ scroll · a scope · s stats · l back · q quit",
		"↑↓ · a · s · l · q"))
	return title + "\n" + dim.Render(strings.Repeat("─", m.w)) + "\n" + m.vp.View() + "\n" + ansi.Truncate(help, m.w, "…")
}
