package tui

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/charmbracelet/x/ansi"

	"dispatch/internal/stats"
	"dispatch/internal/store"
)

// statsRanges are the windows the stats screen cycles through with t.
var statsRanges = []struct {
	label string
	d     time.Duration
}{{"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}, {"all time", 0}}

func (m *model) statsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "s", "esc", "left", "h":
		m.statsOn = false
		m.render()
		m.reveal()
		return m, nil
	case "l":
		m.statsOn, m.learnOn = false, true
		m.renderLearn()
		m.vp.GotoTop()
		return m, nil
	case "t":
		m.statsRange = (m.statsRange + 1) % len(statsRanges)
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

// scoped returns every run in the current repository scope inside the range.
func (m *model) scoped() ([]store.Record, time.Time) {
	var since time.Time
	if d := statsRanges[m.statsRange].d; d > 0 {
		since = m.now.Add(-d)
	}
	var out []store.Record
	for _, r := range m.all {
		if !m.match(r) || (!since.IsZero() && r.CreatedAt.Before(since)) {
			continue
		}
		out = append(out, r)
	}
	return out, since
}

func bar(frac float64, w int, c lipgloss.Color) string {
	n := int(frac*float64(w) + 0.5)
	n = max(0, min(n, w))
	return lipgloss.NewStyle().Foreground(c).Render(strings.Repeat("█", n)) + dim.Render(strings.Repeat("░", w-n))
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f) }

func pctColor(f float64) lipgloss.Color {
	switch {
	case f >= 90:
		return cOK
	case f >= 60:
		return cWarn
	}
	return cBad
}

func msp(p *int64) string {
	if p == nil {
		return "—"
	}
	return fmtDur(time.Duration(*p) * time.Millisecond)
}

func heading(s string, w int) string {
	return lipgloss.NewStyle().Bold(true).Foreground(cAccent).Render(s) + " " + dim.Render(strings.Repeat("─", max(w-lipgloss.Width(s)-1, 0)))
}

func (m *model) renderStats() {
	if !m.ready {
		return
	}
	w := max(m.w-2, 40)
	recs, since := m.scoped()
	var b strings.Builder
	out := func(s string) { b.WriteString(s + "\n") }

	out(heading("Dispatch usage", w))
	out("")
	out(m.usageTable())
	out(dim.Render("  Only dispatch runs: runs, and the time agents spent working on them. Not account-wide quota."))
	out("")
	// overview
	var done, failed, stopped, live, verified, verifyFail, fallback, files, ins, del int
	var durs []int64
	for _, r := range recs {
		switch r.Status {
		case store.Completed:
			done++
			if r.Verification == "verification_failed" {
				verifyFail++
			}
		case store.Failed, store.Orphaned:
			failed++
		case store.Cancelled, store.TimedOut:
			stopped++
		default:
			live++
		}
		if r.Verification == "verified" {
			verified++
		}
		if len(r.Attempts) > 0 {
			fallback++
		}
		if r.FilesChanged != nil {
			files += *r.FilesChanged
		}
		if r.Insertions != nil {
			ins += *r.Insertions
		}
		if r.Deletions != nil {
			del += *r.Deletions
		}
		if r.Status == store.Completed {
			if ms, ok := r.ObservedMs(); ok {
				durs = append(durs, ms)
			}
		}
	}
	finished := len(recs) - live
	rate := func(n, d int) float64 {
		if d == 0 {
			return 0
		}
		return 100 * float64(n) / float64(d)
	}
	okRate := rate(done-verifyFail, finished)
	out(heading("Overview", w))
	if len(recs) == 0 {
		out("")
		out(dim.Render("  No runs in this range."))
	} else {
		out("")
		p50, ok50 := pctl(durs, 0.5)
		p90, _ := pctl(durs, 0.9)
		rows := [][]string{
			{"Runs", text.Bold(true).Render(fmt.Sprint(len(recs))) + dim.Render(fmt.Sprintf("  (%d running)", live))},
			{"Completed", lipgloss.NewStyle().Foreground(cOK).Render(fmt.Sprint(done))},
			{"Failed", lipgloss.NewStyle().Foreground(cBad).Render(fmt.Sprint(failed))},
			{"Stopped", lipgloss.NewStyle().Foreground(cOrange).Render(fmt.Sprint(stopped)) + dim.Render("  cancelled or timed out")},
			{"Success rate", bar(okRate/100, 16, pctColor(okRate)) + " " + text.Bold(true).Render(pct(okRate))},
			{"Verified", text.Render(pct(rate(verified, done))) + dim.Render("  of completed runs")},
			{"Fallback", text.Render(fmt.Sprint(fallback)) + dim.Render("  runs, "+pct(rate(fallback, len(recs)))+" of all")},
		}
		if ok50 {
			rows = append(rows, []string{"Duration", dim.Render("p50 ") + text.Render(fmtDur(time.Duration(p50)*time.Millisecond)) + dim.Render("   p90 ") + text.Render(fmtDur(time.Duration(p90)*time.Millisecond))})
		}
		rows = append(rows, []string{"Changes", text.Render(fmt.Sprintf("%d files", files)) + "  " +
			lipgloss.NewStyle().Foreground(cOK).Render(fmt.Sprintf("+%d", ins)) + " " + lipgloss.NewStyle().Foreground(cBad).Render(fmt.Sprintf("−%d", del))})
		out(grid([]string{"metric", "value"}, rows))
	}

	// activity
	if len(recs) > 0 {
		out("")
		out(heading("Activity", w))
		out("")
		for _, l := range m.activity(recs, since, w) {
			out(l)
		}
	}

	// breakdowns
	for _, by := range []struct{ key, title string }{{"agent", "Agents"}, {"model", "Models"}, {"provider", "Providers"}, {"tier", "Tiers"}} {
		rep, err := stats.Compute(recs, stats.Options{By: by.key, Since: since, Now: m.now})
		if err != nil || len(rep.Rows) == 0 {
			continue
		}
		out("")
		out(heading(by.title, w))
		out("")
		out(m.table(rep.Rows, agentTime(recs, by.key), w))
	}
	m.vp.SetContent(strings.TrimRight(b.String(), "\n"))
}

// grid draws a bordered table with a dim frame.
func grid(headers []string, rows [][]string, right ...int) string {
	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderStyle(dim).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			st := lipgloss.NewStyle().Padding(0, 1)
			if row == table.HeaderRow {
				st = st.Foreground(cDim)
			}
			for _, c := range right {
				if c == col {
					st = st.Align(lipgloss.Right)
				}
			}
			return st
		})
	return t.Render()
}

// table renders one breakdown: share bar, success, first-try, latency, trouble.
func (m *model) table(rows []stats.Row, spent map[string]int64, w int) string {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Runs > rows[j].Runs })
	total := 0
	for _, r := range rows {
		total += r.Runs
	}
	var body [][]string
	for _, r := range rows {
		key := r.Key
		if key == "desconhecido" {
			key = "unknown"
		}
		share := float64(r.Runs) / float64(max(total, 1))
		body = append(body, []string{
			text.Bold(true).Render(ansi.Truncate(key, 28, "…")),
			fmt.Sprint(r.Runs),
			bar(share, 10, cAccent),
			lipgloss.NewStyle().Foreground(pctColor(r.SuccessPct)).Render(pct(r.SuccessPct)),
			dim.Render(pct(r.FirstTryPct)),
			msp(r.P50Ms), msp(r.P90Ms),
			text.Render(fmtDur(time.Duration(spent[r.Key]) * time.Millisecond)),
			counter(r.Fallbacks, cWarn), counter(r.RateLimits, cBad),
		})
	}
	return grid([]string{"", "runs", "share", "success", "1st try", "p50", "p90", "agent time", "fallbacks", "rate limits"}, body, 1, 3, 4, 5, 6, 7, 8, 9)
}

func counter(n int, c lipgloss.Color) string {
	s := fmt.Sprint(n)
	if n == 0 {
		return dim.Render(s)
	}
	return lipgloss.NewStyle().Foreground(c).Render(s)
}

// activity draws runs per day (or per hour for 24h) as a column chart.
func (m *model) activity(recs []store.Record, since time.Time, w int) []string {
	hourly := !since.IsZero() && m.now.Sub(since) <= 24*time.Hour+time.Minute
	unit, layout := 24*time.Hour, "01-02"
	if hourly {
		unit, layout = time.Hour, "15h"
	}
	n := 14
	if hourly {
		n = 24
	}
	if !since.IsZero() {
		n = int(m.now.Sub(since)/unit) + 1
	}
	n = max(1, min(n, max(w-4, 10), 60))
	end := m.now.Truncate(unit)
	if !hourly {
		y, mo, d := m.now.Date()
		end = time.Date(y, mo, d, 0, 0, 0, 0, m.now.Location())
	}
	counts := make([]int, n)
	bad := make([]int, n)
	for _, r := range recs {
		t := r.CreatedAt.Local()
		if hourly {
			t = t.Truncate(time.Hour)
		} else {
			y, mo, d := t.Date()
			t = time.Date(y, mo, d, 0, 0, 0, 0, end.Location())
		}
		i := n - 1 - int(math.Round(end.Sub(t).Hours()/unit.Hours()))
		if i < 0 || i >= n {
			continue
		}
		counts[i]++
		if r.Status == store.Failed || r.Status == store.Orphaned || r.Verification == "verification_failed" {
			bad[i]++
		}
	}
	peak := 1
	for _, c := range counts {
		peak = max(peak, c)
	}
	const rows = 5
	lines := make([]string, 0, rows+2)
	for row := rows; row >= 1; row-- {
		var sb strings.Builder
		for i := range counts {
			frac := float64(counts[i]) / float64(peak) * rows
			ch := " "
			if frac >= float64(row) || (frac > float64(row-1) && row == 1) {
				ch = "█"
			} else if frac > float64(row-1) {
				ch = []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇"}[min(int((frac-float64(row-1))*7), 6)]
			}
			col := cAccent
			if bad[i] > 0 && ch != " " {
				col = cBad
			}
			sb.WriteString(lipgloss.NewStyle().Foreground(col).Render(ch))
		}
		label := "     "
		if row == rows {
			label = fmt.Sprintf("%4d ", peak)
		}
		lines = append(lines, "  "+dim.Render(label)+sb.String())
	}
	first, last := end.Add(-time.Duration(n-1)*unit), end
	axis := first.Format(layout)
	right := last.Format(layout)
	gap := max(n-len(axis)-len(right), 1)
	lines = append(lines, "       "+dim.Render(axis+strings.Repeat(" ", gap)+right))
	lines = append(lines, "       "+dim.Render("runs per "+map[bool]string{true: "hour", false: "day"}[hourly])+dim.Render("  ·  red = had failures"))
	return lines
}

func pctl(xs []int64, p float64) (int64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[min(int(p*float64(len(s)-1)+0.5), len(s)-1)], true
}

func (m *model) statsView() string {
	scope := "all repositories"
	if m.root != "" && !m.showAll {
		scope = fmtBase(m.root)
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(cAccent).Render("▍dispatch") + dim.Render("  statistics · "+scope)
	rng := lipgloss.NewStyle().Foreground(cRun).Render("range " + statsRanges[m.statsRange].label)
	gap := max(m.w-lipgloss.Width(title)-lipgloss.Width(rng)-1, 1)
	scopeKey := "all repos"
	if m.showAll {
		scopeKey = "this repo"
	}
	help := dim.Render(fit(m.w,
		"↑↓ pgup/pgdn scroll · t range · a "+scopeKey+" · l learning · s/esc back · q quit",
		"↑↓ scroll · t range · a scope · s back · q quit",
		"↑↓ · t · a · s · q"))
	return title + strings.Repeat(" ", gap) + rng + "\n" + dim.Render(strings.Repeat("─", m.w)) + "\n" + m.vp.View() + "\n" + ansi.Truncate(help, m.w, "…")
}

// agentTime sums how long agents worked per group key, counting every
// attempt, so fallbacks that burned time still show up.
func agentTime(recs []store.Record, by string) map[string]int64 {
	out := map[string]int64{}
	for _, r := range recs {
		key := map[string]string{"agent": r.Agent, "model": r.Model, "provider": r.Provider, "tier": r.Tier}[by]
		if key == "" {
			key = "desconhecido"
		}
		for _, a := range r.Attempts {
			if a.DurationMs != nil {
				out[key] += *a.DurationMs
			}
		}
		if ms, ok := r.ObservedMs(); ok && r.Status != store.Pending && r.Status != store.Running {
			out[key] += ms
		}
	}
	return out
}

// usageTable is dispatch's own footprint: runs and agent time per window,
// independent of anything else the user runs on the same accounts.
func (m *model) usageTable() string {
	var rows [][]string
	for _, w := range []struct {
		label string
		d     time.Duration
	}{{"Last 24h", 24 * time.Hour}, {"Last 7 days", 7 * 24 * time.Hour}, {"Last 30 days", 30 * 24 * time.Hour}, {"All time", 0}} {
		var runs, files int
		var spent int64
		for _, r := range m.all {
			if !m.match(r) || (w.d > 0 && m.now.Sub(r.CreatedAt) > w.d) {
				continue
			}
			runs++
			if r.FilesChanged != nil {
				files += *r.FilesChanged
			}
			spent += agentTime([]store.Record{r}, "agent")[coalesce(r.Agent, "desconhecido")]
		}
		rows = append(rows, []string{text.Bold(true).Render(w.label), fmt.Sprint(runs),
			fmtDur(time.Duration(spent) * time.Millisecond), fmt.Sprint(files)})
	}
	return grid([]string{"window", "runs", "agent time", "files changed"}, rows, 1, 2, 3)
}
