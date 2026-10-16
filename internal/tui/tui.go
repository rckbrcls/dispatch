// Package tui is the live, per-repository run viewer: one card per dispatch
// call, newest on top, with routing, timing, verification, history and the
// agent's live log.
package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"dispatch/internal/runner"
	"dispatch/internal/stats"
	"dispatch/internal/store"
)

// Options configures the viewer. Root is the repository to follow; empty
// shows every run.
type Options struct {
	Store store.Store
	Root  string
	// Exe is the dispatch binary used for the cancel key.
	Exe string
}

// Run blocks until the user quits.
func Run(o Options) error {
	m := &model{opts: o, open: map[string]bool{}, logs: map[string]logTail{}, follow: true, showAll: o.Root == ""}
	m.root = canon(o.Root)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

type tickMsg time.Time

type logTail struct {
	size  int64
	lines []string
}

type model struct {
	opts    Options
	root    string
	recs    []store.Record // filtered, newest first
	total   int
	ev      map[string]stats.ModelEvidence
	sel     int
	follow  bool
	open    map[string]bool
	logs    map[string]logTail
	vp      viewport.Model
	w, h    int
	showAll bool
	sig     string
	now     time.Time
	flash   string
	starts  []int
	ends    []int
	ready   bool
	// full-screen log view
	logID     string
	logFollow bool
	// statistics screen
	statsOn    bool
	learnOn    bool
	statsRange int
	all        []store.Record
	// settings screen
	settingsOn bool
	setTab     int
	setRow     int
	setProject bool
	setMsg     string
	edit       *editState
	prompt     *prompt
	setN       int // lines rendered so far in the settings page
	setLine    int // line of the selected row
	editOrig   string
}

func (m *model) Init() tea.Cmd {
	m.refresh(true)
	return tea.Batch(tea.SetWindowTitle("dispatch"), tick())
}

func tick() tea.Cmd {
	return tea.Tick(400*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp = viewport.New(m.w, max(m.h-3, 1))
		m.ready = true
		switch {
		case m.logID != "":
			m.renderLog()
		case m.statsOn:
			m.renderStats()
		case m.learnOn:
			m.renderLearn()
		case m.settingsOn:
			m.renderSettings()
		default:
			m.render()
		}
	case editDoneMsg:
		m.finishEdit(msg)
		return m, nil
	case tickMsg:
		m.refresh(false)
		return m, tick()
	case tea.KeyMsg:
		// Held or fast keys arrive batched ("jjjj"): replay them one by one so
		// shortcuts still match. Text inputs and pastes keep the whole run.
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste && !(m.settingsOn && m.prompt != nil) {
			var cmds []tea.Cmd
			for _, r := range msg.Runes {
				_, c := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
				cmds = append(cmds, c)
			}
			return m, tea.Batch(cmds...)
		}
		if m.logID != "" {
			return m.logKey(msg)
		}
		if m.settingsOn {
			return m.settingsKey(msg)
		}
		if m.statsOn {
			return m.statsKey(msg)
		}
		if m.learnOn {
			return m.learnKey(msg)
		}
		switch msg.String() {
		case ",":
			m.openSettings()
			return m, nil
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "enter", "o":
			if r, ok := m.current(); ok {
				m.logID, m.logFollow = r.ID, isLive(r)
				m.loadLog(r)
				m.renderLog()
			}
			return m, nil
		case "up", "k":
			m.move(-1)
		case "down", "j":
			m.move(1)
		case "g", "home":
			m.move(-len(m.recs))
		case "G", "end":
			m.move(len(m.recs))
		case " ", "right":
			if r, ok := m.current(); ok {
				m.open[r.ID] = !m.open[r.ID]
			}
		case "l":
			m.learnOn = true
			m.renderLearn()
			m.vp.GotoTop()
			return m, nil
		case "s":
			m.statsOn = true
			m.renderStats()
			m.vp.GotoTop()
			return m, nil
		case "a":
			if m.root != "" {
				m.showAll = !m.showAll
				m.refresh(true)
			}
		case "c":
			if r, ok := m.current(); ok && (r.Status == store.Running || r.Status == store.Pending) && m.opts.Exe != "" {
				_ = exec.Command(m.opts.Exe, "cancel", r.ID).Start()
				m.flash = "cancelling " + short(r.ID) + "…"
			}
		}
		m.render()
		m.reveal()
	}
	return m, nil
}

func (m *model) current() (store.Record, bool) {
	if m.sel < 0 || m.sel >= len(m.recs) {
		return store.Record{}, false
	}
	return m.recs[m.sel], true
}

func (m *model) move(d int) {
	if len(m.recs) == 0 {
		return
	}
	m.sel = min(max(m.sel+d, 0), len(m.recs)-1)
	m.follow = m.sel == 0
	m.flash = ""
}

func canon(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

func within(dir, root string) bool {
	if dir == "" {
		return false
	}
	for _, d := range []string{dir, canon(dir)} {
		if d == root || strings.HasPrefix(d, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (m *model) match(r store.Record) bool {
	if m.showAll || m.root == "" {
		return true
	}
	return within(r.Repo, m.root) || within(r.Dir, m.root) || within(r.Worktree, m.root)
}

func fileSig(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", st.Size(), st.ModTime().UnixNano())
}

// refresh reloads records only when the store file changed, and always
// refreshes live log tails and the clock.
func (m *model) refresh(force bool) {
	m.now = time.Now()
	if sig := fileSig(m.opts.Store.Path); force || sig != m.sig {
		m.sig = sig
		all, err := m.opts.Store.List()
		if err == nil {
			m.ev = stats.EvidenceByTierModel(all)
			m.total = len(all)
			m.all = all
			var keep []store.Record
			for _, r := range all {
				if m.match(r) {
					keep = append(keep, r)
				}
			}
			var selID string
			if r, ok := m.current(); ok {
				selID = r.ID
			}
			m.recs = keep
			if m.follow || selID == "" {
				m.sel = 0
			} else {
				for i, r := range m.recs {
					if r.ID == selID {
						m.sel = i
					}
				}
			}
			m.sel = min(m.sel, max(len(m.recs)-1, 0))
		}
	}
	for _, r := range m.recs {
		if m.open[r.ID] {
			m.loadLog(r)
		}
	}
	if m.ready && m.logID != "" {
		m.renderLog()
		return
	}
	if m.ready && m.settingsOn {
		return // static page: the tick must not repaint cards over it
	}
	if m.ready && m.statsOn {
		m.renderStats()
		return
	}
	if m.ready && m.learnOn {
		m.renderLearn()
		return
	}
	if m.ready {
		m.render()
		if m.follow {
			m.vp.GotoTop()
		} else {
			m.reveal()
		}
	}
}

// shortenPaths makes agent transcripts relative to the run's worktree and
// home, so long absolute paths do not drown the output.
func shortenPaths(s string, r store.Record) string {
	for _, root := range []string{r.Worktree, r.Dir} {
		if root = strings.TrimRight(root, "/"); root != "" {
			s = strings.ReplaceAll(s, root+"/", "")
			s = strings.ReplaceAll(s, root, ".")
		}
	}
	if h, err := os.UserHomeDir(); err == nil && h != "/" {
		s = strings.ReplaceAll(s, h, "~")
	}
	return s
}

func (m *model) loadLog(r store.Record) {
	p := runner.AgentLogPath(m.opts.Store, r.ID)
	st, err := os.Stat(p)
	if err != nil {
		return
	}
	if lt, ok := m.logs[r.ID]; ok && lt.size == st.Size() {
		return
	}
	f, err := os.Open(p)
	if err != nil {
		return
	}
	defer f.Close()
	const tail = 4 << 20
	off := int64(0)
	if st.Size() > tail {
		off = st.Size() - tail
	}
	buf := make([]byte, st.Size()-off)
	n, _ := f.ReadAt(buf, off)
	text := ansi.Strip(strings.ReplaceAll(string(buf[:n]), "\r", ""))
	text = shortenPaths(text, r)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if off > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	lines = prettyAll(lines)
	m.logs[r.ID] = logTail{size: st.Size(), lines: lines}
}

// --- styles ---

var (
	cBorder = lipgloss.Color("240")
	cDim    = lipgloss.Color("245")
	cText   = lipgloss.Color("252")
	cAccent = lipgloss.Color("141")
	cOK     = lipgloss.Color("78")
	cBad    = lipgloss.Color("203")
	cWarn   = lipgloss.Color("221")
	cRun    = lipgloss.Color("81")
	cOrange = lipgloss.Color("215")
	cPink   = lipgloss.Color("212")

	dim    = lipgloss.NewStyle().Foreground(cDim)
	text   = lipgloss.NewStyle().Foreground(cText)
	accent = lipgloss.NewStyle().Foreground(cAccent)
)

func statusStyle(r store.Record) (string, lipgloss.Color) {
	switch r.Status {
	case store.Running:
		return "RUNNING", cRun
	case store.Pending:
		return "PENDING", cDim
	case store.Completed:
		if r.Verification == "verification_failed" {
			return "VERIFY FAIL", cBad
		}
		return "COMPLETED", cOK
	case store.Failed:
		return "FAILED", cBad
	case store.Cancelled:
		return "CANCELLED", cOrange
	case store.TimedOut:
		return "TIMED OUT", cOrange
	case store.Orphaned:
		return "ORPHANED", cPink
	}
	return string(r.Status), cDim
}

func short(id string) string {
	if i := strings.LastIndex(id, "-"); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func ago(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	if d < 5*time.Second {
		return "just now"
	}
	return fmtDur(d.Truncate(time.Second)) + " ago"
}

func isLive(r store.Record) bool { return r.Status == store.Running || r.Status == store.Pending }

func (m *model) elapsed(r store.Record) string {
	if isLive(r) {
		start := r.StartedAt
		if start.IsZero() {
			start = r.CreatedAt
		}
		return fmtDur(m.now.Sub(start).Truncate(100 * time.Millisecond))
	}
	if ms, ok := r.ObservedMs(); ok {
		return fmtDur(time.Duration(ms) * time.Millisecond)
	}
	return "—"
}

func ms(p *int64) string {
	if p == nil {
		return "—"
	}
	return fmtDur(time.Duration(*p) * time.Millisecond)
}

func oneLine(s string, w int) string {
	s = strings.Join(strings.Fields(s), " ")
	return ansi.Truncate(s, w, "…")
}

// card renders one record. inner is the usable width inside the border.
func (m *model) card(r store.Record, selected bool, inner int) string {
	label, color := statusStyle(r)
	badge := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(color).Padding(0, 1).Render(label)
	dot := ""
	if isLive(r) {
		dot = " " + lipgloss.NewStyle().Foreground(color).Render([]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}[int(m.now.UnixMilli()/100)%10])
	}
	left := badge + dot + " " + accent.Bold(true).Render(short(r.ID)) + dim.Render("  "+ago(r.CreatedAt, m.now))
	right := lipgloss.NewStyle().Foreground(color).Bold(true).Render(m.elapsed(r))
	gap := max(inner-lipgloss.Width(left)-lipgloss.Width(right), 1)
	lines := []string{left + strings.Repeat(" ", gap) + right}

	for i, l := range strings.Split(ansi.Wrap(strings.Join(strings.Fields(r.Prompt), " "), inner-2, " "), "\n") {
		if i >= 2 {
			lines[len(lines)-1] = ansi.Truncate(lines[len(lines)-1], inner-1, "") + dim.Render("…")
			break
		}
		lead := "  "
		if i == 0 {
			lead = dim.Render("❯ ")
		}
		lines = append(lines, lead+text.Render(l))
	}
	lines = append(lines, "")

	field := func(label, value string) string { return dim.Render(label+" ") + value }

	// route
	tier := coalesce(r.Tier, "—")
	if r.ExecutionTier != "" && r.ExecutionTier != r.Tier {
		tier += " → " + r.ExecutionTier
	}
	var who []string
	for _, v := range []string{r.Agent, r.Model, r.Provider} {
		if v != "" {
			who = append(who, text.Bold(true).Render(v))
		}
	}
	if len(who) > 0 {
		lines = append(lines, row("Model", who, inner)...)
	}
	cfg := []string{field("tier", text.Render(tier))}
	if r.Effort != "" {
		cfg = append(cfg, field("effort", text.Render(r.Effort)))
	}
	if r.Resolved != nil && r.Resolved.Priority != "" {
		cfg = append(cfg, field("priority", text.Render(r.Resolved.Priority)))
	}
	lines = append(lines, row("Route", cfg, inner)...)
	if rq := r.Requested; rq != nil {
		var parts []string
		for _, kv := range [][2]string{{"provider", rq.Provider}, {"model", rq.Model}, {"agent", rq.Agent}, {"effort", rq.Effort}} {
			if kv[1] != "" {
				parts = append(parts, text.Render(kv[0]+"="+kv[1]))
			}
		}
		if rq.Relax {
			parts = append(parts, text.Render("relax"))
		}
		if len(parts) > 0 {
			lines = append(lines, row("Filters", parts, inner)...)
		}
	}

	lines = append(lines, "")

	// timing
	t := []string{field("queue", text.Render(ms(r.QueuedMs))), field("exec", text.Render(ms(r.RunMs)))}
	if r.VerifyMs > 0 {
		t = append(t, field("verify", text.Render(fmtDur(time.Duration(r.VerifyMs)*time.Millisecond))))
	}
	lines = append(lines, row("Timing", t, inner)...)

	// result
	var res []string
	if r.FilesChanged != nil {
		s := text.Render(fmt.Sprintf("%d files", *r.FilesChanged))
		if r.Insertions != nil && r.Deletions != nil {
			s += " " + lipgloss.NewStyle().Foreground(cOK).Render(fmt.Sprintf("+%d", *r.Insertions)) + " " +
				lipgloss.NewStyle().Foreground(cBad).Render(fmt.Sprintf("−%d", *r.Deletions))
		}
		res = append(res, s)
	}
	if r.Verification != "" {
		vc := cDim
		switch r.Verification {
		case "verified":
			vc = cOK
		case "verification_failed", "error":
			vc = cBad
		}
		res = append(res, lipgloss.NewStyle().Foreground(vc).Render(r.Verification))
	}
	if r.ExitClass != "" && r.ExitClass != "ok" {
		res = append(res, field("class", lipgloss.NewStyle().Foreground(cWarn).Render(r.ExitClass)))
	}
	if len(res) > 0 {
		lines = append(lines, row("Result", res, inner)...)
	}

	// model history
	if r.Model != "" {
		key := stats.EvidenceKey(coalesce(r.ExecutionTier, r.Tier), r.Model)
		if e, ok := m.ev[key]; ok && e.Samples > 0 {
			h := []string{dim.Render(fmt.Sprintf("n=%d", e.Samples))}
			if e.Verified > 0 {
				h = append(h, dim.Render(fmt.Sprintf("%.0f%% verified", e.VerifiedRate*100)))
			}
			if e.P50Ms != nil {
				h = append(h, dim.Render("p50 "+ms(e.P50Ms)))
			}
			lines = append(lines, row("History", h, inner)...)
		}
	}
	if r.Branch != "" || r.Note != "" {
		lines = append(lines, "")
	}
	if r.Branch != "" {
		lines = append(lines, row("Branch", []string{text.Render(r.Branch)}, inner)...)
	}
	if r.Note != "" {
		lines = append(lines, row("Note", []string{text.Render(oneLine(r.Note, 300))}, inner)...)
	}

	if len(r.Attempts) > 0 {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(cWarn).Render("Fallbacks"))
		for _, a := range r.Attempts {
			d := ""
			if a.DurationMs != nil {
				d = "  " + ms(a.DurationMs)
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(cWarn).Render("  ↳ ")+text.Bold(true).Render(a.Agent+"/"+a.Model)+dim.Render(d+"  ·  "+coalesce(a.ExitClass, "failed")))
			if a.Error != "" {
				for _, l := range strings.Split(ansi.Wrap(oneLine(a.Error, 160), inner-6, " "), "\n") {
					lines = append(lines, "      "+dim.Render(l))
				}
			}
		}
	}
	if r.Error != "" {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(cBad).Render("Error"))
		for _, l := range strings.Split(ansi.Wrap(oneLine(r.Error, 240), inner-2, " "), "\n") {
			lines = append(lines, "  "+text.Render(l))
		}
	}

	if m.open[r.ID] {
		lines = append(lines, dim.Render(strings.Repeat("─", max(inner, 1))))
		n := max(m.h/2, 8)
		for _, l := range m.logLines(r, n) {
			lines = append(lines, ansi.Truncate(strings.ReplaceAll(l, "\t", "  "), inner, "…"))
		}
	}

	bc := cBorder
	if selected {
		bc = color
	}
	st := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(bc).Padding(1, 2).Width(inner + 4)
	if selected {
		st = st.Border(lipgloss.ThickBorder())
	}
	return st.Render(strings.Join(lines, "\n"))
}

// row lays items out under a fixed-width label, wrapping only between items
// and indenting continuation lines under the first value.
func row(label string, items []string, inner int) []string {
	const lw = 10
	avail := max(inner-lw, 10)
	sep := dim.Render("  ·  ")
	var out []string
	cur := ""
	for _, it := range items {
		switch {
		case cur == "":
			cur = it
		case lipgloss.Width(cur)+lipgloss.Width(sep)+lipgloss.Width(it) <= avail:
			cur += sep + it
		default:
			out = append(out, cur)
			cur = it
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	for i, l := range out {
		lead := strings.Repeat(" ", lw)
		if i == 0 {
			lead = dim.Render(label + strings.Repeat(" ", max(lw-len(label), 1)))
		}
		out[i] = lead + ansi.Truncate(l, avail, "…")
	}
	return out
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (m *model) logLines(r store.Record, n int) []string {
	var src []string
	if lt, ok := m.logs[r.ID]; ok {
		src = lt.lines
	} else if r.Output != "" {
		src = strings.Split(strings.TrimRight(ansi.Strip(r.Output), "\n"), "\n")
	}
	if len(src) == 0 {
		if isLive(r) {
			return []string{dim.Render("waiting for agent output…")}
		}
		return []string{dim.Render("no log recorded")}
	}
	if len(src) > n {
		src = src[len(src)-n:]
	}
	return src
}

func (m *model) render() {
	if !m.ready {
		return
	}
	inner := m.w - 6
	var b strings.Builder
	m.starts, m.ends = m.starts[:0], m.ends[:0]
	line := 0
	for i, r := range m.recs {
		c := m.card(r, i == m.sel, inner)
		m.starts = append(m.starts, line)
		n := strings.Count(c, "\n") + 1
		line += n
		m.ends = append(m.ends, line-1)
		b.WriteString(c)
		b.WriteString("\n\n")
		line += 2
	}
	if len(m.recs) == 0 {
		where := "in this repository"
		if m.showAll {
			where = "yet"
		}
		b.WriteString("\n" + dim.Render("  No dispatch calls "+where+". Run `dispatch run …` and it shows up here live.") + "\n")
	}
	m.vp.SetContent(b.String())
}

// reveal scrolls so the selected card is visible.
func (m *model) reveal() {
	if !m.ready || m.sel >= len(m.starts) {
		return
	}
	s, e := m.starts[m.sel], m.ends[m.sel]
	switch {
	case s < m.vp.YOffset:
		m.vp.SetYOffset(s)
	case e >= m.vp.YOffset+m.vp.Height:
		m.vp.SetYOffset(max(e-m.vp.Height+1, min(s, e)))
		if e-s+1 > m.vp.Height {
			m.vp.SetYOffset(s)
		}
	}
}

func (m *model) View() string {
	if !m.ready {
		return "loading…"
	}
	if m.logID != "" {
		return m.logView()
	}
	if m.settingsOn {
		return m.settingsView()
	}
	if m.statsOn {
		return m.statsView()
	}
	if m.learnOn {
		return m.learnView()
	}
	var run, ok, bad int
	for _, r := range m.recs {
		switch {
		case isLive(r):
			run++
		case r.Status == store.Completed && r.Verification != "verification_failed":
			ok++
		default:
			bad++
		}
	}
	scope := "all repositories"
	if m.root != "" && !m.showAll {
		scope = filepath.Base(m.root)
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(cAccent).Render("▍dispatch") + dim.Render("  "+scope)
	counts := lipgloss.NewStyle().Foreground(cRun).Render(fmt.Sprintf("● %d running", run)) + dim.Render("  ") +
		lipgloss.NewStyle().Foreground(cOK).Render(fmt.Sprintf("✓ %d ok", ok)) + dim.Render("  ") +
		lipgloss.NewStyle().Foreground(cBad).Render(fmt.Sprintf("✗ %d failed", bad))
	gap := max(m.w-lipgloss.Width(title)-lipgloss.Width(counts)-1, 1)
	head := title + strings.Repeat(" ", gap) + counts
	rule := dim.Render(strings.Repeat("─", m.w))
	scopeKey := "all repos"
	if m.showAll {
		scopeKey = "this repo"
	}
	help := fit(m.w,
		"↑↓ navigate · enter full output · space preview · s stats · l learning · , settings · c cancel · a "+scopeKey+" · q quit",
		"↑↓ move · enter output · space peek · s stats · l learn · , settings · c cancel · a "+scopeKey+" · q quit",
		"↑↓ · enter · space · s · l · , · c · a · q")
	if m.flash != "" {
		help = lipgloss.NewStyle().Foreground(cWarn).Render(m.flash) + dim.Render("  ·  ") + dim.Render(help)
	} else {
		help = dim.Render(help)
	}
	return head + "\n" + rule + "\n" + m.vp.View() + "\n" + ansi.Truncate(help, m.w, "…")
}

// --- full-screen verbose output ---

func (m *model) logRec() (store.Record, bool) {
	for _, r := range m.recs {
		if r.ID == m.logID {
			return r, true
		}
	}
	return store.Record{}, false
}

func (m *model) logKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "q", "esc", "enter", "left", "h":
		m.logID = ""
		m.render()
		m.reveal()
		return m, nil
	case "f":
		m.logFollow = !m.logFollow
		if m.logFollow {
			m.vp.GotoBottom()
		}
		return m, nil
	case "g", "home":
		m.logFollow = false
		m.vp.GotoTop()
		return m, nil
	case "G", "end":
		m.vp.GotoBottom()
		m.logFollow = true
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	if !m.vp.AtBottom() {
		m.logFollow = false
	}
	return m, cmd
}

func (m *model) renderLog() {
	r, ok := m.logRec()
	if !ok {
		return
	}
	m.loadLog(r)
	src := m.logLines(r, 1<<30)
	var b strings.Builder
	for _, l := range src {
		b.WriteString(wrapHang(strings.ReplaceAll(l, "\t", "  "), m.w-1))
		b.WriteString("\n")
	}
	m.vp.SetContent(strings.TrimRight(b.String(), "\n"))
	if m.logFollow {
		m.vp.GotoBottom()
	}
}

func (m *model) logView() string {
	r, ok := m.logRec()
	if !ok {
		return "run not found"
	}
	label, color := statusStyle(r)
	badge := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(color).Padding(0, 1).Render(label)
	left := badge + " " + accent.Bold(true).Render(short(r.ID)) + dim.Render("  "+strings.Join(nonEmpty(r.Agent, r.Model, r.Effort), " · "))
	right := lipgloss.NewStyle().Foreground(color).Bold(true).Render(m.elapsed(r))
	gap := max(m.w-lipgloss.Width(left)-lipgloss.Width(right)-1, 1)
	follow := "following"
	if !m.logFollow {
		follow = "paused (f to follow)"
	}
	help := dim.Render(fit(m.w,
		"↑↓ pgup/pgdn scroll · g top · G bottom · f follow · esc back  ·  "+follow,
		"↑↓ scroll · g/G ends · f follow · esc back  ·  "+follow,
		"↑↓ · f · esc"))
	return left + strings.Repeat(" ", gap) + right + "\n" + dim.Render(strings.Repeat("─", m.w)) + "\n" + m.vp.View() + "\n" + ansi.Truncate(help, m.w, "…")
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// fit returns the first (longest) option that fits in w columns.
func fit(w int, options ...string) string {
	for _, o := range options {
		if lipgloss.Width(o) <= w {
			return o
		}
	}
	return options[len(options)-1]
}

func fmtBase(p string) string { return filepath.Base(p) }

// wrapHang wraps a transcript line and indents the continuation under the
// text that follows the line's marker (▶, ●, │ …).
func wrapHang(l string, w int) string {
	if lipgloss.Width(l) <= w || w < 20 {
		return l
	}
	plain := ansi.Strip(l)
	lead := len(plain) - len(strings.TrimLeft(plain, " "))
	rest := []rune(strings.TrimLeft(plain, " "))
	hang := lead
	pre := ""
	if len(rest) >= 2 && rest[1] == ' ' {
		hang += 2
		if rest[0] == '│' {
			pre = strings.Repeat(" ", lead) + "│ "
		}
	}
	if pre == "" {
		pre = strings.Repeat(" ", hang)
	}
	wrapped := strings.Split(ansi.Wrap(l, w-hang, " "), "\n")
	// first line keeps its marker; ansi.Wrap measured it against the narrower width, which is safe
	for i := 1; i < len(wrapped); i++ {
		wrapped[i] = dim.Render(pre) + wrapped[i]
	}
	return strings.Join(wrapped, "\n")
}
