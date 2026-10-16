package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"dispatch/internal/adapter"
	"dispatch/internal/config"
	"dispatch/internal/subagent"
	"dispatch/internal/tiers"
)

// Settings tabs. The first three edit files; the last one is read-only.
const (
	tabConfig = iota
	tabTiers
	tabSubagents
	tabAgents
)

var settingsTabs = []string{"Config", "Tiers", "Subagents", "Agents"}

var priorities = []string{"", "speed", "balanced", "quality"}

// editState tracks one $EDITOR session on a temp copy of a settings file.
// The real file is replaced only after the copy validates.
type editState struct {
	target string // file being edited
	tmp    string // working copy, kept while the text is invalid
	perm   os.FileMode
	err    string // validation error from the last attempt
	check  func(tmp string) error
}

type editDoneMsg struct{ err error }

// prompt is a one-line text input for values that are quicker to type than
// to edit as JSON (a list entry, a timeout, a new profile name).
type prompt struct {
	label string
	val   []rune
	apply func(string) error
}

// typed applies one key to the input: runes append, backspace deletes,
// ctrl+u clears.
func (p *prompt) typed(msg tea.KeyMsg) {
	switch msg.Type {
	case tea.KeyRunes, tea.KeySpace:
		p.val = append(p.val, msg.Runes...)
	case tea.KeyBackspace:
		if len(p.val) > 0 {
			p.val = p.val[:len(p.val)-1]
		}
	case tea.KeyCtrlU:
		p.val = nil
	}
}

func (m *model) ask(label, initial string, apply func(string) error) {
	m.prompt = &prompt{label: label, val: []rune(initial), apply: apply}
}

// saveValidated writes b to a temp file beside target, validates that file
// with check and only then renames it over target.
func saveValidated(target string, b []byte, perm os.FileMode, check func(string) error) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".save-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Chmod(perm)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := check(name); err != nil {
		return err
	}
	return os.Rename(name, target)
}

func (m *model) report(err error, ok string) {
	if err != nil {
		m.setMsg = "! " + err.Error()
		return
	}
	m.setMsg = ok
}

// ruleList is the allow/deny list a config row points at.
func ruleList(f *config.File, row int) (*[]string, string) {
	r, name := &f.Allow, "allow"
	if row >= 5 {
		r, name = &f.Deny, "deny"
	}
	switch (row - 2) % 3 {
	case 0:
		return &r.Providers, name + ".providers"
	case 1:
		return &r.Models, name + ".models"
	}
	return &r.Agents, name + ".agents"
}

const configRows = 8 // priority, discover, allow x3, deny x3

func (m *model) rowCount() int {
	switch m.setTab {
	case tabConfig:
		return configRows
	case tabTiers:
		return len(m.tierRows())
	case tabSubagents:
		p, _ := subagent.Load(m.settingsPath())
		return len(p)
	}
	return 0
}

type tierRef struct {
	tier adapter.Tier
	idx  int
}

var tierOrder = []adapter.Tier{adapter.TierEasy, adapter.TierNormal, adapter.TierHard}

func (m *model) tierRows() []tierRef {
	t, err := tiers.Load(m.settingsPath())
	if err != nil {
		return nil
	}
	var out []tierRef
	for _, tier := range tierOrder {
		for i := range t.Tiers[tier] {
			out = append(out, tierRef{tier, i})
		}
	}
	return out
}

func (m *model) saveTiers(t tiers.Table) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return saveValidated(m.settingsPath(), append(b, '\n'), 0o644, func(p string) error { _, err := tiers.Load(p); return err })
}

func (m *model) saveProfiles(profiles map[string]subagent.Profile) error {
	b, err := json.MarshalIndent(subagent.File{Agents: profiles}, "", "  ")
	if err != nil {
		return err
	}
	return saveValidated(m.settingsPath(), append(b, '\n'), 0o644, checkSubagents)
}

func cycle(cur string, opts []string) string {
	for i, o := range opts {
		if o == cur {
			return opts[(i+1)%len(opts)]
		}
	}
	return opts[0]
}

func bump(n, delta int) int { return ((n+delta)%6 + 6) % 6 } // 0 (unset) .. 5

// inlineKey handles the per-tab editing keys. It reports whether it used k.
func (m *model) inlineKey(k string) bool {
	switch m.setTab {
	case tabTiers:
		return m.tiersKey(k)
	case tabSubagents:
		return m.subagentsKey(k)
	}
	return false
}

func (m *model) tiersKey(k string) bool {
	rows := m.tierRows()
	path := m.settingsPath()
	t, err := tiers.Load(path)
	if err != nil {
		return false
	}
	var cur *tierRef
	if m.setRow < len(rows) {
		cur = &rows[m.setRow]
	}
	switch k {
	case "a":
		tier := adapter.TierNormal
		if cur != nil {
			tier = cur.tier
		}
		m.ask("new "+string(tier)+" entry (agent model)", "", func(v string) error {
			f := strings.Fields(v)
			if len(f) != 2 {
				return errors.New("want: <agent> <model>")
			}
			t.Tiers[tier] = append(t.Tiers[tier], tiers.Entry{Agent: f[0], Model: f[1]})
			return m.saveTiers(t)
		})
		return true
	}
	if cur == nil {
		return false
	}
	es := t.Tiers[cur.tier]
	switch k {
	case "s", "S":
		d := 1
		if k == "S" {
			d = -1
		}
		es[cur.idx].Speed = bump(es[cur.idx].Speed, d)
	case "t", "T":
		d := 1
		if k == "T" {
			d = -1
		}
		es[cur.idx].Strength = bump(es[cur.idx].Strength, d)
	case "K", "J":
		j := cur.idx - 1
		if k == "J" {
			j = cur.idx + 1
		}
		if j < 0 || j >= len(es) {
			return true
		}
		es[cur.idx], es[j] = es[j], es[cur.idx]
		if k == "J" {
			m.setRow++
		} else {
			m.setRow--
		}
	case "x":
		t.Tiers[cur.tier] = append(es[:cur.idx:cur.idx], es[cur.idx+1:]...)
		m.setRow = max(m.setRow-1, 0)
	case "g", "m", "o", "f":
		label, get, set := tierField(k, &es[cur.idx])
		m.ask(label, get, func(v string) error {
			set(v)
			return m.saveTiers(t)
		})
		return true
	default:
		return false
	}
	m.report(m.saveTiers(t), "saved "+shortHome(path))
	return true
}

func tierField(k string, e *tiers.Entry) (label, cur string, set func(string)) {
	switch k {
	case "g":
		return "agent", e.Agent, func(v string) { e.Agent = v }
	case "m":
		return "model", e.Model, func(v string) { e.Model = v }
	case "o":
		return "provider (empty = derive from model)", e.Provider, func(v string) { e.Provider = v }
	}
	return "efforts (comma separated)", strings.Join(e.Efforts, ","), func(v string) { e.Efforts = splitList(v) }
}

func splitList(v string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, f)
	}
	return out
}

func profileField(k string, p *subagent.Profile) (label, cur string, set func(string)) {
	switch k {
	case "g":
		return "agent", p.Agent, func(v string) { p.Agent = v }
	case "m":
		return "model", p.Model, func(v string) { p.Model = v }
	case "o":
		return "provider", p.Provider, func(v string) { p.Provider = v }
	case "f":
		return "effort", p.Effort, func(v string) { p.Effort = v }
	case "i":
		return "instructions", p.Instructions, func(v string) { p.Instructions = v }
	case "w":
		return "prefer agents (comma separated)", strings.Join(p.Prefer, ","), func(v string) { p.Prefer = splitList(v) }
	}
	return "avoid agents (comma separated)", strings.Join(p.Avoid, ","), func(v string) { p.Avoid = splitList(v) }
}

func (m *model) subagentsKey(k string) bool {
	path := m.settingsPath()
	profiles, err := subagent.Load(path)
	if err != nil {
		return false
	}
	if profiles == nil {
		profiles = map[string]subagent.Profile{}
	}
	names := subagent.Names(profiles)
	if k == "n" {
		m.ask("new profile name", "", func(v string) error {
			v = strings.TrimSpace(v)
			if v == "" || strings.ContainsAny(v, " \t") {
				return errors.New("name must be one word")
			}
			if _, ok := profiles[v]; ok {
				return errors.New(v + " already exists")
			}
			profiles[v] = subagent.Profile{Tier: "normal"}
			return m.saveProfiles(profiles)
		})
		return true
	}
	if m.setRow >= len(names) {
		return false
	}
	name := names[m.setRow]
	p := profiles[name]
	switch k {
	case "enter", " ":
		p.Tier = cycle(p.Tier, []string{"", "easy", "normal", "hard"})
	case "r":
		p.ReadOnly = !p.ReadOnly
	case "v":
		p.NoVerify = !p.NoVerify
	case "y":
		p.Priority = cycle(p.Priority, priorities)
	case "t":
		m.ask(name+" timeout (e.g. 15m, empty = none)", p.Timeout, func(v string) error {
			p.Timeout = strings.TrimSpace(v)
			profiles[name] = p
			return m.saveProfiles(profiles)
		})
		return true
	case "z":
		p.Relax = !p.Relax
	case "x":
		delete(profiles, name)
		m.setRow = max(m.setRow-1, 0)
	case "g", "m", "o", "f", "i", "w", "b":
		label, get, set := profileField(k, &p)
		m.ask(name+" "+label, get, func(v string) error {
			set(v)
			profiles[name] = p
			return m.saveProfiles(profiles)
		})
		return true
	default:
		return false
	}
	profiles[name] = p
	m.report(m.saveProfiles(profiles), "saved "+shortHome(path))
	return true
}

func (m *model) home() string { return filepath.Dir(m.opts.Store.Path) }

// settingsPath is the file behind a tab, honouring the global/project scope.
func (m *model) settingsPath() string {
	switch m.setTab {
	case tabTiers:
		return filepath.Join(m.home(), "tiers.json")
	case tabSubagents:
		if m.setProject && m.root != "" {
			return subagent.ProjectPath(m.root)
		}
		return filepath.Join(m.home(), "agents.json")
	default:
		if m.setProject && m.root != "" {
			return filepath.Join(m.root, ".dispatch", "config.json")
		}
		return config.Path(m.home())
	}
}

func (m *model) openSettings() {
	m.settingsOn = true
	m.setMsg = ""
	m.renderSettings()
	m.vp.GotoTop()
}

func (m *model) settingsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if m.prompt != nil {
		switch k {
		case "enter":
			pr := m.prompt
			m.prompt = nil
			m.report(pr.apply(strings.TrimSpace(string(pr.val))), "saved "+shortHome(m.settingsPath()))
			m.renderSettings()
		case "esc":
			m.prompt = nil
		case "ctrl+c":
			return m, tea.Quit
		default:
			m.prompt.typed(msg)
		}
		return m, nil
	}
	if m.edit != nil { // validation failed: the editor is closed, text is kept
		switch k {
		case "enter", "e":
			return m, m.runEditor()
		case "esc", "q", "d":
			_ = os.Remove(m.edit.tmp)
			m.edit = nil
			m.setMsg = "edit discarded"
			m.renderSettings()
		case "ctrl+c":
			return m, tea.Quit
		}
		return m, nil
	}
	if m.setTab != tabConfig && m.inlineKey(k) {
		m.renderSettings()
		return m, nil
	}
	switch k {
	case "ctrl+c":
		return m, tea.Quit
	case "q", "esc", ",":
		m.settingsOn = false
		m.render()
		m.reveal()
		return m, nil
	case "tab", "right", "l":
		m.setTab, m.setRow = (m.setTab+1)%len(settingsTabs), 0
	case "shift+tab", "left", "h":
		m.setTab, m.setRow = (m.setTab+len(settingsTabs)-1)%len(settingsTabs), 0
	case "1", "2", "3", "4":
		m.setTab = int(k[0] - '1')
		m.setRow = 0
	case "p":
		if m.root != "" && (m.setTab == tabConfig || m.setTab == tabSubagents) {
			m.setProject = !m.setProject
			m.setRow = 0
		}
	case "up", "k":
		m.setRow = max(m.setRow-1, 0)
	case "down", "j":
		m.setRow = min(m.setRow+1, max(m.rowCount()-1, 0))
	case "enter", " ":
		if m.setTab == tabConfig {
			m.configEnter()
		}
	case "e":
		if m.setTab != tabAgents {
			return m, m.beginEdit()
		}
	default:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	m.setMsg = ""
	m.renderSettings()
	return m, nil
}

// configEnter acts on the selected config row: values toggle at once, list
// rows ask for an entry to add (typing one already there removes it).
func (m *model) configEnter() {
	if m.setRow < 2 {
		m.toggleSetting()
		return
	}
	path := m.settingsPath()
	f, err := config.Load(path)
	if err != nil {
		m.setMsg = "! " + err.Error()
		return
	}
	_, name := ruleList(&f, m.setRow)
	m.ask(name+": value to add (an existing value is removed)", "", func(v string) error {
		if v == "" {
			return errors.New("empty value")
		}
		f, err := config.Load(path)
		if err != nil {
			return err
		}
		dst, name := ruleList(&f, m.setRow)
		if slices.Contains(*dst, v) {
			*dst = slices.DeleteFunc(*dst, func(x string) bool { return x == v })
		} else {
			*dst = append(*dst, v)
		}
		key := strings.SplitN(name, ".", 2)[0]
		if key == "allow" {
			return config.WriteKey(path, key, f.Allow)
		}
		return config.WriteKey(path, key, f.Deny)
	})
}

// toggleSetting changes priority or discover in place and saves at once.
func (m *model) toggleSetting() {
	path := m.settingsPath()
	f, err := config.Load(path)
	if err != nil {
		m.setMsg = "! " + err.Error()
		return
	}
	switch m.setRow {
	case 0:
		i := 0
		for j, p := range priorities {
			if p == f.Priority {
				i = j
			}
		}
		f.Priority = priorities[(i+1)%len(priorities)]
		if err := config.Validate(f); err != nil {
			m.setMsg = "! " + err.Error()
			return
		}
		err = config.WriteKey(path, "priority", f.Priority)
	case 1:
		f.Discover = !f.Discover
		err = config.WriteKey(path, "discover", f.Discover)
	}
	if err != nil {
		m.setMsg = "! " + err.Error()
		return
	}
	m.setMsg = "saved " + shortHome(path)
}

// --- $EDITOR ---

// editorCommand builds the editor invocation from $VISUAL/$EDITOR (which may
// carry flags, e.g. "code -w"), falling back to vi.
func editorCommand(path string) *exec.Cmd {
	ed := strings.TrimSpace(os.Getenv("VISUAL"))
	if ed == "" {
		ed = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	parts := strings.Fields(ed)
	if len(parts) == 0 {
		parts = []string{"vi"}
	}
	return exec.Command(parts[0], append(parts[1:], path)...)
}

// beginEdit copies the file (or a starter template when it does not exist)
// to a temp file next to it and opens that in the editor.
func (m *model) beginEdit() tea.Cmd {
	target := m.settingsPath()
	perm := os.FileMode(0o644)
	var seed []byte
	var check func(string) error
	switch m.setTab {
	case tabTiers:
		seed = tiers.DefaultJSON()
		check = func(p string) error { _, err := tiers.Load(p); return err }
	case tabSubagents:
		seed = []byte(subagent.Example)
		check = checkSubagents
	default:
		seed = []byte("{\n  \"priority\": \"balanced\"\n}\n")
		perm = 0o600
		check = func(p string) error { _, err := config.Load(p); return err }
	}
	if b, err := os.ReadFile(target); err == nil {
		seed = b
		if st, err := os.Stat(target); err == nil {
			perm = st.Mode().Perm()
		}
	} else if !os.IsNotExist(err) {
		m.setMsg = "! " + err.Error()
		m.renderSettings()
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		m.setMsg = "! " + err.Error()
		m.renderSettings()
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".edit-*.json")
	if err == nil {
		_, err = tmp.Write(seed)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		m.setMsg = "! " + err.Error()
		m.renderSettings()
		return nil
	}
	m.edit = &editState{target: target, tmp: tmp.Name(), perm: perm, check: check}
	m.editOrig = string(seed)
	return m.runEditor()
}

func (m *model) runEditor() tea.Cmd {
	return tea.ExecProcess(editorCommand(m.edit.tmp), func(err error) tea.Msg { return editDoneMsg{err} })
}

// checkSubagents loads the file and validates every profile against the
// agents dispatch knows, like `dispatch subagents --check`.
func checkSubagents(path string) error {
	profiles, err := subagent.Load(path)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, a := range adapter.Known() {
		known[a.Name()] = true
	}
	var problems []string
	for _, name := range subagent.Names(profiles) {
		problems = append(problems, subagent.Validate(name, profiles[name], known)...)
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// finishEdit validates the edited copy. Valid text replaces the real file
// atomically; invalid text keeps the copy and shows the error so the editor
// can be reopened on it.
func (m *model) finishEdit(msg editDoneMsg) {
	e := m.edit
	if e == nil {
		return
	}
	if msg.err != nil {
		_ = os.Remove(e.tmp)
		m.edit = nil
		m.setMsg = "! editor failed: " + msg.err.Error()
		m.renderSettings()
		return
	}
	b, err := os.ReadFile(e.tmp)
	if err != nil {
		m.edit = nil
		m.setMsg = "! " + err.Error()
		m.renderSettings()
		return
	}
	if string(b) == m.editOrig {
		_ = os.Remove(e.tmp)
		m.edit = nil
		m.setMsg = "no changes"
		m.renderSettings()
		return
	}
	if err := e.check(e.tmp); err != nil {
		e.err = err.Error()
		m.renderSettings()
		return
	}
	if err := os.Chmod(e.tmp, e.perm); err == nil {
		err = os.Rename(e.tmp, e.target)
		if err != nil {
			e.err = err.Error()
			m.renderSettings()
			return
		}
	}
	m.edit = nil
	m.setMsg = "saved " + shortHome(e.target)
	m.renderSettings()
}

// --- rendering ---

func shortHome(p string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		return "~" + p[len(h):]
	}
	return p
}

func (m *model) renderSettings() {
	if !m.ready {
		return
	}
	w := max(m.w-2, 40)
	var b strings.Builder
	m.setN = 0
	out := func(s string) { b.WriteString(s + "\n"); m.setN += strings.Count(s, "\n") + 1 }

	var tabs []string
	for i, t := range settingsTabs {
		label := fmt.Sprintf(" %d %s ", i+1, t)
		if i == m.setTab {
			tabs = append(tabs, lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(cAccent).Render(label))
		} else {
			tabs = append(tabs, dim.Render(label))
		}
	}
	out(strings.Join(tabs, " "))
	out("")

	switch m.setTab {
	case tabConfig:
		m.settingsConfig(out, w)
	case tabTiers:
		m.settingsTiers(out, w)
	case tabSubagents:
		m.settingsSubagents(out, w)
	default:
		m.settingsAgents(out, w)
	}

	if m.edit != nil && m.edit.err != "" {
		out("")
		out(lipgloss.NewStyle().Foreground(cBad).Bold(true).Render("✗ not saved — invalid"))
		out(wrapHang("  "+m.edit.err, w))
		out(dim.Render("  your text is kept: enter reopens the editor · esc discards it"))
	} else if m.setMsg != "" {
		out("")
		col := cOK
		if strings.HasPrefix(m.setMsg, "!") {
			col = cBad
		}
		out(lipgloss.NewStyle().Foreground(col).Render(m.setMsg))
	}
	m.vp.SetContent(strings.TrimRight(b.String(), "\n"))
	// keep the selected row on screen instead of jumping back to the top
	switch {
	case m.setRow == 0:
		m.vp.GotoTop()
	case m.setLine < m.vp.YOffset:
		m.vp.SetYOffset(m.setLine)
	case m.setLine >= m.vp.YOffset+m.vp.Height:
		m.vp.SetYOffset(m.setLine - m.vp.Height + 1)
	}
}

func (m *model) scopeLabel() string {
	if m.setProject && m.root != "" {
		return "project · " + fmtBase(m.root)
	}
	return "global"
}

func (m *model) settingsConfig(out func(string), w int) {
	path := m.settingsPath()
	out(heading("Config", w))
	out(dim.Render("  " + shortHome(path) + "  (" + m.scopeLabel() + ")"))
	out("")
	f, err := config.Load(path)
	if err != nil {
		out(lipgloss.NewStyle().Foreground(cBad).Render("  ✗ " + err.Error()))
		out(dim.Render("  press e to fix it in your editor"))
		return
	}
	rows := [][2]string{{"priority", coalesce(f.Priority, "(unset → balanced)")}, {"discover", fmt.Sprint(f.Discover)}}
	for i := 2; i < configRows; i++ {
		dst, name := ruleList(&f, i)
		rows = append(rows, [2]string{name, list(*dst)})
	}
	for i, r := range rows {
		if i == 2 {
			out("")
			out(heading("Policy", w))
			out(dim.Render("  enter adds a value; typing one that exists removes it · deny always wins"))
			out("")
		}
		cur, val := "  ", r[1]
		if i == m.setRow {
			m.setLine = m.setN
			cur = accent.Render("▸ ")
			if i < 2 {
				val = lipgloss.NewStyle().Bold(true).Render(val)
			}
		}
		out(fmt.Sprintf("%s%-16s %s", cur, r[0], val))
	}
	out("")
	out(dim.Render("  enter changes the selected row and saves it · e edits the whole file as text"))
	if m.root != "" {
		out(dim.Render("  p switches between global and this project's config"))
	}
}

func list(s []string) string {
	if len(s) == 0 {
		return dim.Render("—")
	}
	return strings.Join(s, ", ")
}

func (m *model) settingsTiers(out func(string), w int) {
	path := m.settingsPath()
	out(heading("Tiers", w))
	_, statErr := os.Stat(path)
	if statErr != nil {
		out(dim.Render("  " + shortHome(path) + "  (not created: built-in table in use; e starts from it)"))
	} else {
		out(dim.Render("  " + shortHome(path)))
	}
	out("")
	t, err := tiers.Load(path)
	if err != nil {
		out(lipgloss.NewStyle().Foreground(cBad).Render("  ✗ " + err.Error()))
		out(dim.Render("  press e to fix it in your editor"))
		return
	}
	var rows [][]string
	n := 0
	for _, tier := range tierOrder {
		for i, e := range t.Tiers[tier] {
			name := ""
			if i == 0 {
				name = string(tier)
			}
			mark := " "
			if n == m.setRow {
				mark = accent.Render("▸")
			}
			n++
			rows = append(rows, []string{mark, name, e.Agent, e.Model, coalesce(e.Provider, "—"), prio(e.Speed), prio(e.Strength), strings.Join(e.Efforts, ",")})
		}
	}
	m.setLine = m.setN + 3 + m.setRow
	out(grid([]string{"", "tier", "agent", "model", "provider", "speed", "strength", "efforts"}, rows, 5, 6))
	out(dim.Render("  s/S speed ±  t/T strength ±  J/K reorder  a add  x remove\n  g agent  m model  o provider  f efforts · order = preference · e edits the file"))
	if ps := t.CapabilityProblems(adapter.Known()); len(ps) > 0 {
		out("")
		for _, p := range ps {
			out(lipgloss.NewStyle().Foreground(cWarn).Render(wrapHang("  ⚠ "+p, w)))
		}
	}
}

func prio(n int) string {
	if n == 0 {
		return "—"
	}
	return fmt.Sprint(n)
}

func (m *model) settingsSubagents(out func(string), w int) {
	path := m.settingsPath()
	out(heading("Subagents", w))
	out(dim.Render("  " + shortHome(path) + "  (" + m.scopeLabel() + ")"))
	out("")
	profiles, err := subagent.Load(path)
	if err != nil {
		out(lipgloss.NewStyle().Foreground(cBad).Render("  ✗ " + err.Error()))
		out(dim.Render("  press e to fix it in your editor"))
		return
	}
	if len(profiles) == 0 {
		out(dim.Render("  no profiles yet · n creates one · e starts from an example"))
		return
	}
	known := map[string]bool{}
	for _, a := range adapter.Known() {
		known[a.Name()] = true
	}
	var rows [][]string
	var problems []string
	for i, name := range subagent.Names(profiles) {
		p := profiles[name]
		mark := " "
		if i == m.setRow {
			mark = accent.Render("▸")
		}
		ro := ""
		if p.ReadOnly {
			ro = "yes"
		}
		rows = append(rows, []string{mark, name, coalesce(p.Tier, "—"), coalesce(p.Priority, "—"), ro, coalesce(p.Timeout, "—"), oneLine(p.Instructions, 36)})
		problems = append(problems, subagent.Validate(name, p, known)...)
	}
	sort.Strings(problems)
	m.setLine = m.setN + 3 + m.setRow
	out(grid([]string{"", "name", "tier", "priority", "readonly", "timeout", "instructions"}, rows))
	for _, p := range problems {
		out(lipgloss.NewStyle().Foreground(cWarn).Render(wrapHang("  ⚠ "+p, w)))
	}
	out(dim.Render("  enter tier  y priority  r readonly  v no-verify  z relax  n new  x remove\n  t timeout  g agent  m model  o provider  f effort  i instructions  w prefer  b avoid\n  e edits the file · p scope"))
}

func (m *model) settingsAgents(out func(string), w int) {
	out(heading("Agents", w))
	out(dim.Render("  CLIs dispatch can route to (read-only; `dispatch doctor` probes them)"))
	out("")
	var rows [][]string
	for _, a := range adapter.Known() {
		path, state := "—", "not installed"
		if p, err := exec.LookPath(a.Binary()); err == nil {
			path, state = shortHome(p), "installed"
		}
		rows = append(rows, []string{a.Name(), a.Binary(), state, path})
	}
	out(grid([]string{"agent", "binary", "state", "path"}, rows))
}

func (m *model) settingsView() string {
	scope := "all repositories"
	if m.root != "" && !m.showAll {
		scope = fmtBase(m.root)
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(cAccent).Render("▍dispatch") + dim.Render("  settings · "+scope)
	var help string
	switch {
	case m.prompt != nil:
		help = "enter confirm · esc cancel"
	case m.edit != nil:
		help = "enter reopen editor · esc discard"
	case m.setTab == tabConfig:
		help = fit(m.w,
			"tab/1-4 section · ↑↓ select · enter change · e edit JSON · p scope · , back · q quit",
			"tab section · ↑↓ · enter · e edit · p scope · , back",
			"tab · ↑↓ · enter · e · p · ,")
	case m.setTab == tabAgents:
		help = fit(m.w, "tab/1-4 section · , back · q quit", "tab · , back")
	default:
		help = fit(m.w,
			"tab/1-4 section · ↑↓ select · keys below · e edit in $EDITOR · , back · q quit",
			"tab section · ↑↓ · e edit · , back",
			"tab · ↑↓ · e · ,")
	}
	body := m.vp.View()
	if m.prompt != nil {
		body += "\n" + accent.Render("▸ "+m.prompt.label+": ") + string(m.prompt.val) + accent.Render("▏")
	}
	return title + "\n" + dim.Render(strings.Repeat("─", m.w)) + "\n" + body + "\n" + ansi.Truncate(dim.Render(help), m.w, "…")
}
