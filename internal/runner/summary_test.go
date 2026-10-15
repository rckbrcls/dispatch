package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dispatch/internal/adapter"
	"dispatch/internal/store"
	"dispatch/internal/tiers"
	"dispatch/internal/verify"
)

func TestEffortAndProvider(t *testing.T) {
	if EffortOf("gemini-3.8-flash-high") != "high" || EffortOf("gpt-5.6-sol-xhigh") != "xhigh" || EffortOf("grok-4.7-medium") != "medium" {
		t.Fatal(EffortOf("gemini-3.8-flash-high"), EffortOf("gpt-5.6-sol-xhigh"), EffortOf("grok-4.7-medium"))
	}
	if EffortOf("grok-4.7") != "" || EffortOf("grok-4.7-build-fast") != "" || EffortOf("composer-2.5") != "" {
		t.Fatal("suffix that is not an effort must stay unknown")
	}
	cases := []struct{ agent, model, want string }{
		{"antigravity", "gemini-3.8-flash-low", "google"},
		{"cursor", "grok-4.7-medium", "xai"},
		{"codex", "gpt-5.6-luna", "openai"},
		{"cursor", "composer-2.5", "cursor"},
		{"codex", "", "openai"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := ProviderOf(c.agent, c.model); got != c.want {
			t.Errorf("ProviderOf(%s,%s)=%q want %q", c.agent, c.model, got, c.want)
		}
	}
}

func TestClassifyExit(t *testing.T) {
	if Classify(store.Record{Status: store.Failed, Attempts: []store.Attempt{{ExitClass: "rate_limit"}}}) != "rate_limit" {
		t.Fatal("rate_limit")
	}
	if Classify(store.Record{Status: store.Failed, Error: "panic: boom"}) != "crash" {
		t.Fatal("crash")
	}
	if Classify(store.Record{Status: store.Failed, Error: "merge conflict in f.go"}) != "merge_conflict" {
		t.Fatal("merge")
	}
	rec := store.Record{
		Status: store.Completed, Verification: "verification_failed",
		Verify: &verify.Result{Checks: []verify.CheckResult{{Name: "conflicts", Status: verify.CheckFailed}}},
	}
	if Classify(rec) != "merge_conflict" {
		t.Fatal("conflict check")
	}
	rec.Verify.Checks = append(rec.Verify.Checks, verify.CheckResult{Name: "test", Status: verify.CheckFailed})
	if Classify(rec) != "verify_fail" {
		t.Fatal("mixed failure stays verify_fail")
	}
	if Classify(store.Record{Status: store.Completed, Verification: "verified"}) != "ok" {
		t.Fatal("ok")
	}
	if Classify(store.Record{Status: store.Completed, Note: "commit failed: CONFLICT (content)"}) != "merge_conflict" {
		t.Fatal("commit conflict")
	}
}

func TestRunSummaryOmitsAgentText(t *testing.T) {
	ms := int64(1500)
	files, ins, del := 2, 10, 3
	rec := store.Record{
		ID: "id1", Status: store.Completed, Agent: "codex", Model: "gpt-5.6-sol-xhigh",
		Tier: "hard", Provider: "openai", Effort: "xhigh",
		Output:       "AGENT_FREE_TEXT_SHOULD_NOT_LEAK",
		Verification: "verified", Branch: "dispatch/id1", Worktree: "/tmp/wt",
		RunMs: &ms, FilesChanged: &files, Insertions: &ins, Deletions: &del,
		Attempts: []store.Attempt{{Agent: "grok", Model: "grok-4.7", Error: "rate limit"}},
	}
	s := Summarize(rec)
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "AGENT_FREE_TEXT") {
		t.Fatal(string(b))
	}
	if s.Schema != SummarySchema || s.Status != store.Completed || s.Agent != "codex" || s.Model != "gpt-5.6-sol-xhigh" || s.Tier != "hard" {
		t.Fatalf("%+v", s)
	}
	if s.DurationMs == nil || *s.DurationMs != 1500 || s.Verification != "verified" || s.Attempts != 2 {
		t.Fatalf("%+v", s)
	}
	if s.FilesChanged == nil || *s.FilesChanged != 2 || *s.Insertions != 10 || *s.Deletions != 3 {
		t.Fatal("files")
	}
	if s.Details["agent"] != "dispatch result id1 --section agent" {
		t.Fatal(s.Details)
	}
	text := s.Text()
	if strings.Count(text, "\n") != 3 || strings.Contains(text, "AGENT_FREE_TEXT") {
		t.Fatalf("%q", text)
	}
	if !strings.Contains(text, "files=2 +10 -3") || !strings.Contains(text, "verification=verified") {
		t.Fatal(text)
	}

	unknown := Summarize(store.Record{ID: "x", Status: store.Running})
	raw, _ := json.Marshal(unknown)
	for _, field := range []string{"durationMs", "filesChanged", "insertions", "deletions", "queuedMs", "runMs"} {
		if strings.Contains(string(raw), `"`+field+`"`) {
			t.Fatalf("unknown %s leaked: %s", field, raw)
		}
	}
	if !strings.Contains(unknown.Text(), "desconhecido") {
		t.Fatal(unknown.Text())
	}
	z := 0
	zero := Summarize(store.Record{ID: "z", Status: store.Completed, FilesChanged: &z, Insertions: &z, Deletions: &z, RunMs: store.I64(0), FinishedAt: rec.FinishedAt})
	// RunMs 0 is a known duration even without timestamps.
	raw, _ = json.Marshal(zero)
	if !strings.Contains(string(raw), `"filesChanged":0`) || !strings.Contains(string(raw), `"durationMs":0`) {
		t.Fatal(string(raw))
	}
}

func TestTelemetryOnFallback(t *testing.T) {
	var ran []string
	d := deps(t, &ran,
		fake{name: "a", err: errors.New("429 too many requests"), ran: &ran},
		fake{name: "b", out: "UNIQUE_AGENT_TEXT", ran: &ran})
	d.Table = tiers.Table{Tiers: map[adapter.Tier][]tiers.Entry{
		adapter.TierEasy: {
			{Agent: "a", Model: "gemini-3.8-flash-high"},
			{Agent: "b", Model: "gpt-5.6-terra"},
		},
	}}
	rec, err := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Prompt: "p", ReadOnly: true})
	if err != nil || rec.Status != store.Completed || rec.ExitClass != "ok" {
		t.Fatalf("%+v %v", rec, err)
	}
	if rec.Provider != "openai" || rec.Effort != "" || rec.Model != "gpt-5.6-terra" {
		t.Fatalf("provider/effort %+v", rec)
	}
	if rec.QueuedMs == nil || rec.RunMs == nil {
		t.Fatal("missing timings", rec.QueuedMs, rec.RunMs)
	}
	if rec.FilesChanged != nil {
		t.Fatal("readonly run must leave the diff unknown")
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].DurationMs == nil || rec.Attempts[0].ExitClass != "rate_limit" {
		t.Fatalf("attempt %+v", rec.Attempts)
	}
	raw, _ := json.Marshal(Summarize(rec))
	if strings.Contains(string(raw), "UNIQUE_AGENT_TEXT") {
		t.Fatal(string(raw))
	}
	if Summarize(rec).Attempts != 2 {
		t.Fatal(Summarize(rec).Attempts)
	}
}

func TestWriteRunRecordsDiffStat(t *testing.T) {
	t.Setenv("DISPATCH_HOME", t.TempDir())
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=test", "-c", "user.email=test@localhost"}, args...)...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, b)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "base")
	var ran []string
	f := editFake{fake: fake{name: "a", out: "ok\n", ran: &ran}, edit: func(dir string) {
		os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x\n"), 0o644)
	}}
	d := deps(t, &ran, f)
	rec, err := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Prompt: "p", Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	if rec.FilesChanged == nil || *rec.FilesChanged != 1 || rec.Insertions == nil || *rec.Insertions != 1 || rec.Deletions == nil || *rec.Deletions != 0 {
		t.Fatalf("diff %+v files=%v +%v -%v", rec.Status, rec.FilesChanged, rec.Insertions, rec.Deletions)
	}
}
