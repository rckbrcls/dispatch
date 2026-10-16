package tui

import (
	"fmt"
	"github.com/charmbracelet/bubbles/viewport"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dispatch/internal/store"
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
