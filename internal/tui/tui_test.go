package tui

import (
	"fmt"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dispatch/internal/store"
	"dispatch/internal/subagent"
	"dispatch/internal/tiers"
)

func TestPrettyCodexEvents(t *testing.T) {
	cmd := `{"type":"item.completed","item":{"type":"command_execution","command":"go test","exit_code":1,"aggregated_output":"FAIL x"}}`
	cases := [][2]string{
		{`{"type":"item.completed","item":{"type":"agent_message","text":"feito"}}`, "feito"},
		{`{"type":"item.started","item":{"type":"command_execution","command":"go test"}}`, "$ go test"},
		{cmd, "exit 1"},
		{cmd, "│ FAIL x"},
		{`{"type":"item.completed","item":{"type":"reasoning","text":"pensando alto"}}`, "pensando alto"},
		{`{"type":"turn.failed","error":{"message":"deu ruim"}}`, "deu ruim"},
		{`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}`, "10 tokens in"},
		{"texto cru", "texto cru"},
	}
	for _, c := range cases {
		if got := strings.Join(pretty(c[0]), "\n"); !strings.Contains(got, c[1]) {
			t.Errorf("pretty(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	if pretty(`{"type":"thread.started","thread_id":"x"}`) != nil {
		t.Error("thread.started should be hidden")
	}
}

func TestExpandedCardShowsLiveLog(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Path: filepath.Join(dir, "runs.jsonl")}
	rec := store.Record{ID: "20260101-000000-abcd1234", Status: store.Running, Tier: "easy", Agent: "codex",
		Model: "m", CreatedAt: time.Now(), Prompt: "do x"}
	if err := st.Append(rec); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(dir, "logs")
	os.MkdirAll(logs, 0o755)
	os.WriteFile(filepath.Join(logs, rec.ID+".agent.log"), []byte("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"hello from the agent\"}}\n"), 0o644)

	m := &model{opts: Options{Store: st}, open: map[string]bool{rec.ID: true}, logs: map[string]logTail{}, follow: true, showAll: true, w: 100, h: 40, ready: true}
	m.refresh(true)
	out := m.card(m.recs[0], true, 96)
	if !strings.Contains(out, "hello from the agent") || !strings.Contains(out, "RUNNING") {
		t.Fatalf("card missing log or status:\n%s", out)
	}
}

func TestFullLogViewShowsWholeTranscript(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Path: filepath.Join(dir, "runs.jsonl")}
	rec := store.Record{ID: "20260101-000000-abcd1234", Status: store.Running, Tier: "easy", Agent: "codex", Model: "m", CreatedAt: time.Now(), Prompt: "p"}
	if err := st.Append(rec); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "logs"), 0o755)
	var log strings.Builder
	for i := 0; i < 200; i++ {
		log.WriteString(`{"type":"item.completed","item":{"type":"agent_message","text":"linha ` + strings.Repeat("x", i%5) + `"}}` + "\n")
	}
	log.WriteString(`{"type":"item.completed","item":{"type":"agent_message","text":"FIM-DO-LOG"}}` + "\n")
	os.WriteFile(filepath.Join(dir, "logs", rec.ID+".agent.log"), []byte(log.String()), 0o644)

	m := &model{opts: Options{Store: st}, open: map[string]bool{}, logs: map[string]logTail{}, follow: true, showAll: true, w: 100, h: 30, ready: true}
	m.vp = viewport.New(100, 27)
	m.refresh(true)
	m.logID, m.logFollow = rec.ID, true
	m.renderLog()
	if v := m.logView(); !strings.Contains(v, "FIM-DO-LOG") {
		t.Fatalf("follow mode should end at the last line:\n%s", v)
	}
	m.vp.GotoTop()
	m.logFollow = false
	if v := m.logView(); strings.Contains(v, "FIM-DO-LOG") {
		t.Fatal("top of a 200-line log must not show the last line")
	}
}

func TestStatsScreenRendersBreakdowns(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Path: filepath.Join(dir, "runs.jsonl")}
	now := time.Now()
	for i, status := range []store.Status{store.Completed, store.Completed, store.Failed} {
		err := st.Append(store.Record{ID: fmt.Sprintf("20260101-00000%d-abcd123%d", i, i), Status: status, Tier: "easy", Agent: "codex",
			Model: "gpt-x", Provider: "openai", CreatedAt: now.Add(-time.Duration(i) * time.Hour), Prompt: "p", RunMs: store.I64(2000)})
		if err != nil {
			t.Fatal(err)
		}
	}
	m := &model{opts: Options{Store: st}, open: map[string]bool{}, logs: map[string]logTail{}, showAll: true, w: 100, h: 40, ready: true}
	m.vp = viewport.New(100, 200)
	m.refresh(true)
	m.statsRange = 3
	m.statsOn = true
	m.renderStats()
	v := m.statsView()
	for _, want := range []string{"statistics", "Overview", "Completed", "Success rate", "Agents", "codex", "Models", "gpt-x", "Providers", "Tiers"} {
		if !strings.Contains(v, want) {
			t.Errorf("stats view missing %q:\n%s", want, v)
		}
	}
	m.statsRange = 0
	m.renderStats() // 24h range must not panic
}

func TestLogViewUpdatesLiveAndGroupsEvents(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Path: filepath.Join(dir, "runs.jsonl")}
	rec := store.Record{ID: "20260101-000000-abcd1234", Status: store.Running, Tier: "easy", Agent: "codex", Model: "m", CreatedAt: time.Now(), Prompt: "p"}
	if err := st.Append(rec); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "logs"), 0o755)
	path := filepath.Join(dir, "logs", rec.ID+".agent.log")
	os.WriteFile(path, []byte(`{"type":"item.started","item":{"type":"command_execution","command":"/bin/zsh -lc 'go test'"}}`+"\n"), 0o644)

	m := &model{opts: Options{Store: st}, open: map[string]bool{}, logs: map[string]logTail{}, follow: true, showAll: true, w: 100, h: 30, ready: true}
	m.vp = viewport.New(100, 27)
	m.refresh(true)
	m.logID, m.logFollow = rec.ID, true
	m.renderLog()
	if v := m.logView(); !strings.Contains(v, "$ go test") || strings.Contains(v, "zsh") {
		t.Fatalf("command should show unwrapped:\n%s", v)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"type":"item.completed","item":{"type":"command_execution","command":"go test","exit_code":0,"aggregated_output":"ok demo"}}` + "\n")
	f.Close()
	m.refresh(false) // next tick, no change to runs.jsonl
	v := m.logView()
	if !strings.Contains(v, "exit 0") || !strings.Contains(v, "ok demo") {
		t.Fatalf("appended output should appear on the next tick:\n%s", v)
	}
	if strings.Contains(prettyJoin(`{"type":"item.started","item":{"type":"command_execution","command":"x"}}`, `{"type":"item.completed","item":{"type":"command_execution","command":"x","exit_code":0}}`), "\n\n  ✓") {
		t.Error("a command and its result should not be separated by a blank line")
	}
}

func prettyJoin(ls ...string) string { return strings.Join(prettyAll(ls), "\n") }

func TestLearnScreenRenders(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Path: filepath.Join(dir, "runs.jsonl")}
	for i := 0; i < 3; i++ {
		err := st.Append(store.Record{ID: fmt.Sprintf("20260101-00000%d-abcd123%d", i, i), Status: store.Completed, Tier: "easy", ExecutionTier: "easy",
			Agent: "codex", Model: "gpt-x", Verification: "verified", CreatedAt: time.Now(), Prompt: "p", RunMs: store.I64(1000)})
		if err != nil {
			t.Fatal(err)
		}
	}
	m := &model{opts: Options{Store: st}, open: map[string]bool{}, logs: map[string]logTail{}, showAll: true, w: 100, h: 40, ready: true}
	m.vp = viewport.New(100, 300)
	m.refresh(true)
	m.learnOn = true
	m.renderLearn()
	v := m.learnView()
	for _, want := range []string{"learning", "Classifier", "warming up", "What it learned", "Calibration", "Learning curve", "Model evidence", "gpt-x", "Rate-limit memory"} {
		if !strings.Contains(v, want) {
			t.Errorf("learning view missing %q", want)
		}
	}
}

func settingsModel(t *testing.T) (*model, string) {
	dir := t.TempDir()
	m := &model{opts: Options{Store: store.Store{Path: filepath.Join(dir, "runs.jsonl")}}, open: map[string]bool{}, logs: map[string]logTail{}, showAll: true, w: 110, h: 40, ready: true}
	m.vp = viewport.New(110, 300)
	m.openSettings()
	return m, dir
}

func TestSettingsInlineToggleSavesConfig(t *testing.T) {
	m, dir := settingsModel(t)
	m.toggleSetting() // priority: unset -> speed
	m.setRow = 1
	m.toggleSetting() // discover -> true
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil || !strings.Contains(string(b), `"speed"`) || !strings.Contains(string(b), `"discover": true`) {
		t.Fatalf("%s %v", b, err)
	}
	m.renderSettings()
	if v := m.settingsView(); !strings.Contains(v, "speed") || !strings.Contains(v, "settings") {
		t.Fatal(v)
	}
}

func TestSettingsEditValidatesBeforeReplacing(t *testing.T) {
	m, dir := settingsModel(t)
	target := filepath.Join(dir, "config.json")
	if err := os.WriteFile(target, []byte(`{"priority":"speed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.setTab = tabConfig
	m.beginEdit() // opens the editor command; we drive the result by hand
	e := m.edit
	if e == nil || e.tmp == target {
		t.Fatalf("%+v", e)
	}
	// invalid priority: file untouched, copy kept, error shown
	os.WriteFile(e.tmp, []byte(`{"priority":"warp"}`), 0o600)
	m.finishEdit(editDoneMsg{})
	if m.edit == nil || m.edit.err == "" {
		t.Fatal("expected validation error")
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "speed") {
		t.Fatalf("target changed: %s", b)
	}
	if v := m.settingsView(); !strings.Contains(v, "reopen") {
		t.Fatal(v)
	}
	// fixed on retry: replaced atomically, temp copy gone
	os.WriteFile(e.tmp, []byte(`{"priority":"quality"}`), 0o600)
	m.finishEdit(editDoneMsg{})
	if m.edit != nil {
		t.Fatal("edit should be finished")
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "quality") {
		t.Fatalf("%s", b)
	}
	if _, err := os.Stat(e.tmp); !os.IsNotExist(err) {
		t.Fatal("temp copy left behind")
	}
}

func TestSettingsTabsRenderAndSubagentCheck(t *testing.T) {
	m, dir := settingsModel(t)
	for tab, want := range map[int]string{tabTiers: "efforts", tabSubagents: "no profiles", tabAgents: "binary"} {
		m.setTab = tab
		m.renderSettings()
		if v := m.settingsView(); !strings.Contains(v, want) {
			t.Errorf("tab %d missing %q:\n%s", tab, want, v)
		}
	}
	bad := filepath.Join(dir, "agents.json")
	os.WriteFile(bad, []byte(`{"agents":{"x":{"tier":"giant"}}}`), 0o644)
	if err := checkSubagents(bad); err == nil || !strings.Contains(err.Error(), "invalid tier") {
		t.Fatalf("%v", err)
	}
	os.Setenv("EDITOR", "code -w")
	t.Cleanup(func() { os.Unsetenv("EDITOR") })
	os.Unsetenv("VISUAL")
	if c := editorCommand("f.json"); len(c.Args) != 3 || c.Args[1] != "-w" {
		t.Fatalf("%v", c.Args)
	}
}

func press(m *model, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		m.Update(msg)
	}
}

func typeText(m *model, s string) {
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

func TestSettingsEditEverythingInline(t *testing.T) {
	m, dir := settingsModel(t)

	// config: add and then remove a deny.models entry
	press(m, "j", "j", "j", "j", "j", "j") // row 6 = deny.models
	press(m, "enter")
	typeText(m, "gpt-bad")
	cfg, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if !strings.Contains(string(cfg), "gpt-bad") || !strings.Contains(string(cfg), `"deny"`) {
		t.Fatalf("%s", cfg)
	}
	press(m, "enter")
	typeText(m, "gpt-bad")
	cfg, _ = os.ReadFile(filepath.Join(dir, "config.json"))
	if strings.Contains(string(cfg), "gpt-bad") {
		t.Fatalf("not removed: %s", cfg)
	}

	// tiers: bump speed, add entry, reorder, edit model
	press(m, "tab")
	press(m, "s")
	press(m, "a")
	typeText(m, "codex gpt-new")
	press(m, "m")
	m.prompt = nil
	b, err := os.ReadFile(filepath.Join(dir, "tiers.json"))
	if err != nil || !strings.Contains(string(b), "gpt-new") || !strings.Contains(string(b), `"speed"`) {
		t.Fatalf("%s %v", b, err)
	}
	press(m, "J")
	if _, err := tiers.Load(filepath.Join(dir, "tiers.json")); err != nil {
		t.Fatal(err)
	}

	// subagents: new profile, cycle tier, readonly, timeout, instructions
	m.setTab, m.setRow = tabSubagents, 0
	press(m, "n")
	typeText(m, "rev")
	press(m, "enter", "r", "t")
	typeText(m, "15m")
	press(m, "i")
	typeText(m, "review the diff")
	ps, err := subagent.Load(filepath.Join(dir, "agents.json"))
	p := ps["rev"]
	if err != nil || p.Tier != "hard" || !p.ReadOnly || p.Timeout != "15m" || p.Instructions != "review the diff" {
		t.Fatalf("%+v %v", p, err)
	}
	press(m, "t")
	typeText(m, "banana")
	if m.setMsg == "" || !strings.HasPrefix(m.setMsg, "!") {
		t.Fatalf("invalid timeout accepted: %q", m.setMsg)
	}
	ps, _ = subagent.Load(filepath.Join(dir, "agents.json"))
	if ps["rev"].Timeout != "15m" {
		t.Fatal("invalid value was saved")
	}
}

func TestSettingsSelectionStaysVisibleInShortWindow(t *testing.T) {
	m, _ := settingsModel(t)
	m.h = 12
	m.vp = viewport.New(110, 8)
	m.renderSettings()
	for i := 0; i < 7; i++ {
		press(m, "j")
	}
	if v := m.settingsView(); !strings.Contains(v, "▸ deny.agents") {
		t.Fatalf("selected row scrolled out of view:\n%s", v)
	}
	for i := 0; i < 7; i++ {
		press(m, "k")
	}
	if v := m.settingsView(); !strings.Contains(v, "▸ priority") {
		t.Fatalf("top row not visible:\n%s", v)
	}
}

func TestSettingsSurvivesRefreshTick(t *testing.T) {
	m, _ := settingsModel(t)
	m.refresh(true)
	m.Update(tickMsg(time.Now()))
	if v := m.settingsView(); !strings.Contains(v, "Config") || strings.Contains(v, "COMPLETED") {
		t.Fatalf("tick repainted the settings page:\n%s", v)
	}
}

func TestBatchedKeysAreReplayed(t *testing.T) {
	m, _ := settingsModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("jjj")})
	if m.setRow != 3 {
		t.Fatalf("row = %d, want 3", m.setRow)
	}
}
