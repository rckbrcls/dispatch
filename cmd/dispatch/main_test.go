package main

import (
	"bytes"
	"dispatch/internal/runner"
	"dispatch/internal/store"
	"dispatch/internal/verify"
	"dispatch/internal/worktree"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dispatch/internal/hook"
	"dispatch/internal/skill"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	outC := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		outC <- buf.String()
	}()

	fn()

	w.Close()
	os.Stdout = old
	return <-outC
}

func setupTestEnv(t *testing.T) (string, string) {
	t.Helper()
	tempDir := t.TempDir()
	dispatchHome := filepath.Join(tempDir, ".dispatch")
	claudeDir := filepath.Join(tempDir, ".claude")

	t.Setenv("HOME", tempDir)
	t.Setenv("DISPATCH_HOME", dispatchHome)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)

	return dispatchHome, claudeDir
}

func TestCLIOnOffStatus(t *testing.T) {
	dispatchHome, claudeDir := setupTestEnv(t)
	settingsPath := filepath.Join(claudeDir, "settings.json")
	skillPath := filepath.Join(claudeDir, "skills", skill.Name, "SKILL.md")

	// Initially, dispatch is off, hook not installed, skill not installed.
	out := captureStdout(t, func() {
		code := statusCmd(nil)
		if code != 0 {
			t.Fatalf("statusCmd returned code %d", code)
		}
	})
	if !strings.Contains(out, "off") || !strings.Contains(out, "not installed") {
		t.Fatalf("unexpected initial status: %s", out)
	}

	// Turn on: sets flag, installs skill if missing, installs hook if missing.
	out = captureStdout(t, func() {
		code := onCmd(nil)
		if code != 0 {
			t.Fatalf("onCmd returned code %d", code)
		}
	})
	if !strings.Contains(out, "on") {
		t.Fatalf("unexpected on output: %s", out)
	}

	// Verify flag exists.
	if !hook.IsEnabled(dispatchHome) {
		t.Fatal("expected dispatch to be enabled after onCmd")
	}

	// Verify skill installed.
	if _, err := os.Stat(skillPath); err != nil {
		t.Fatalf("expected skill file to exist at %s: %v", skillPath, err)
	}

	// Verify hook installed.
	installed, err := hook.IsInstalled(settingsPath)
	if err != nil || !installed {
		t.Fatalf("expected hook to be installed: %v, installed=%v", err, installed)
	}

	// Status should now show on, hook installed, skill installed.
	out = captureStdout(t, func() {
		code := statusCmd(nil)
		if code != 0 {
			t.Fatalf("statusCmd returned code %d", code)
		}
	})
	if !strings.Contains(out, "on") || strings.Contains(out, "not installed") {
		t.Fatalf("status should be fully on and installed: %s", out)
	}

	// Turn off: unsets flag.
	out = captureStdout(t, func() {
		code := offCmd(nil)
		if code != 0 {
			t.Fatalf("offCmd returned code %d", code)
		}
	})
	if !strings.Contains(out, "off") {
		t.Fatalf("unexpected off output: %s", out)
	}
	if hook.IsEnabled(dispatchHome) {
		t.Fatal("expected dispatch to be disabled after offCmd")
	}

	// Status should now show off, but hook and skill remain installed.
	out = captureStdout(t, func() {
		code := statusCmd(nil)
		if code != 0 {
			t.Fatalf("statusCmd returned code %d", code)
		}
	})
	if !strings.Contains(out, "off") || !strings.Contains(out, "installed") {
		t.Fatalf("status should show off with installed hook/skill: %s", out)
	}
}

func TestCLIContextHook(t *testing.T) {
	dispatchHome, _ := setupTestEnv(t)

	// When disabled: prints nothing and exits 0.
	hook.SetEnabled(dispatchHome, false)
	out := captureStdout(t, func() {
		code := contextCmd([]string{"--hook"})
		if code != 0 {
			t.Fatalf("contextCmd returned %d", code)
		}
	})
	if out != "" {
		t.Fatalf("expected empty output when disabled, got: %q", out)
	}

	// When enabled: prints the context block and exits 0.
	hook.SetEnabled(dispatchHome, true)
	out = captureStdout(t, func() {
		code := contextCmd([]string{"--hook"})
		if code != 0 {
			t.Fatalf("contextCmd returned %d", code)
		}
	})
	if out != hook.ContextBlock() {
		t.Fatalf("expected context block %q, got: %q", hook.ContextBlock(), out)
	}
}

func TestCLIHookInstallUninstall(t *testing.T) {
	_, claudeDir := setupTestEnv(t)
	settingsPath := filepath.Join(claudeDir, "settings.json")

	// Install hook
	out := captureStdout(t, func() {
		code := hookCmd([]string{"install"})
		if code != 0 {
			t.Fatalf("hookCmd install returned %d", code)
		}
	})
	if !strings.Contains(out, "+ hook") {
		t.Fatalf("unexpected hook install output: %s", out)
	}

	installed, err := hook.IsInstalled(settingsPath)
	if err != nil || !installed {
		t.Fatalf("expected hook to be installed: %v", err)
	}

	// Duplicate install does not error and does not duplicate
	out = captureStdout(t, func() {
		code := hookCmd([]string{"install"})
		if code != 0 {
			t.Fatalf("second hookCmd install returned %d", code)
		}
	})

	// Uninstall hook
	out = captureStdout(t, func() {
		code := hookCmd([]string{"uninstall"})
		if code != 0 {
			t.Fatalf("hookCmd uninstall returned %d", code)
		}
	})
	if !strings.Contains(out, "- hook") {
		t.Fatalf("unexpected hook uninstall output: %s", out)
	}

	installed, err = hook.IsInstalled(settingsPath)
	if err != nil || installed {
		t.Fatalf("expected hook to be uninstalled: %v, installed=%v", err, installed)
	}
}

func TestOrphanRecoveryCLI(t *testing.T) {
	if os.Getenv("DISPATCH_TEST_DIRTY_CLEAN") == "1" {
		clean([]string{"orphan"})
		return
	}
	setupTestEnv(t)
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@localhost"}, args...)...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	git(root, "init", "-q")
	git(root, "commit", "--allow-empty", "-qm", "base")
	base := git(root, "rev-parse", "HEAD")
	wt := filepath.Join(t.TempDir(), "wt")
	branch, err := worktree.Create(root, wt, "orphan")
	if err != nil {
		t.Fatal(err)
	}
	rec := store.Record{ID: "orphan", Status: store.Orphaned, Repo: root, Worktree: wt, Branch: branch, BaseSHA: base}
	if err := runsStore().Append(rec); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(wt, "work.txt")
	if err := os.WriteFile(file, []byte("work"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestOrphanRecoveryCLI$")
	cmd.Env = append(os.Environ(), "DISPATCH_TEST_DIRTY_CLEAN=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "--force") {
		t.Fatalf("clean should refuse dirty orphan: %s %v", out, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	result := captureStdout(t, func() {
		if code := recoverCmd([]string{"orphan"}); code != 0 {
			t.Fatal(code)
		}
	})
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["status"] != "COMPLETED" || !strings.Contains(decoded["diffStat"].(string), "work.txt") {
		t.Fatal(result)
	}
	captureStdout(t, func() {
		if code := merge([]string{"orphan"}); code != 0 {
			t.Fatal(code)
		}
	})
	if _, err := os.Stat(filepath.Join(root, "work.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestMergeRefusesVerificationFailure(t *testing.T) {
	if os.Getenv("DISPATCH_TEST_MERGE_REFUSE") == "1" {
		merge([]string{"bad"})
		return
	}
	setupTestEnv(t)
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@localhost"}, args...)...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	git(root, "init", "-q", "-b", "main")
	git(root, "commit", "--allow-empty", "-qm", "base")
	wt := filepath.Join(t.TempDir(), "wt")
	branch, err := worktree.Create(root, wt, "bad")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(wt, "add", "-A")
	git(wt, "commit", "-qm", "work")
	rec := store.Record{
		ID: "bad", Status: store.Completed, Repo: root, Worktree: wt, Branch: branch,
		Verification: "verification_failed",
		Verify: &verify.Result{
			Status: verify.StatusFailed,
			Checks: []verify.CheckResult{{
				Name: "test", Status: verify.CheckFailed, Regressions: []string{"pkg.TestX"},
			}},
		},
	}
	if err := runsStore().Append(rec); err != nil {
		t.Fatal(err)
	}
	if err := mergeRefusal(rec, false); err == nil || mergeRefusal(rec, true) != nil {
		t.Fatal("refusal predicate")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMergeRefusesVerificationFailure$")
	cmd.Env = append(os.Environ(), "DISPATCH_TEST_MERGE_REFUSE=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "pkg.TestX") {
		t.Fatalf("merge should refuse: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(wt, "work.txt")); err != nil {
		t.Fatal("refused merge removed the worktree")
	}
	merged := captureStdout(t, func() {
		if code := merge([]string{"--force", "bad"}); code != 0 {
			t.Fatal(code)
		}
	})
	if !strings.Contains(merged, "pkg.TestX") || !strings.Contains(merged, "despite") {
		t.Fatal(merged)
	}
	if _, err := os.Stat(filepath.Join(root, "work.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestVerificationViewAndNotes(t *testing.T) {
	setupTestEnv(t)
	if mergeNote(store.Record{}, false) == "" || mergeNote(store.Record{Verification: "unverified"}, false) == "" {
		t.Fatal("empty and unverified should warn")
	}
	if mergeNote(store.Record{Verification: "error"}, false) == "" {
		t.Fatal("error should warn")
	}
	if mergeNote(store.Record{Verification: "verified"}, false) != "" {
		t.Fatal("verified should not warn")
	}
	regs := make([]string, 12)
	for i := range regs {
		regs[i] = fmt.Sprintf("pkg.Test%d", i)
	}
	rec := store.Record{
		ID: "v1", Status: store.Completed, Verification: "verification_failed",
		Verify: &verify.Result{
			Status:   verify.StatusFailed,
			Warnings: []string{"lockfile changed: go.sum"},
			Checks: []verify.CheckResult{{
				Name: "test", Status: verify.CheckFailed, Regressions: regs,
				Tail: "saw TOKEN=supersecret in log",
			}},
		},
	}
	if err := runsStore().Append(rec); err != nil {
		t.Fatal(err)
	}
	stored, err := runsStore().Get("v1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.Verify.Checks[0].Tail, "supersecret") || !strings.Contains(stored.Verify.Checks[0].Tail, "[REDACTED]") {
		t.Fatalf("tail %q", stored.Verify.Checks[0].Tail)
	}
	comp := view(stored)["verification"].(map[string]any)
	if comp["status"] != "verification_failed" || len(comp["regressions"].([]string)) != 10 {
		t.Fatalf("%#v", comp)
	}
	meta := captureStdout(t, func() {
		if code := result([]string{"--section", "meta", "v1"}); code != 0 {
			t.Fatal(code)
		}
	})
	if strings.Contains(meta, "supersecret") || strings.Contains(meta, "TOKEN=") {
		t.Fatal(meta)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
		t.Fatal(err)
	}
	vfy := decoded["verification"].(map[string]any)
	if got := vfy["regressions"].([]any); len(got) != 10 {
		t.Fatalf("%v", got)
	}
	detail := captureStdout(t, func() {
		if code := result([]string{"--section", "verification", "v1"}); code != 0 {
			t.Fatal(code)
		}
	})
	if !strings.Contains(detail, "pkg.Test11") || !strings.Contains(detail, "[REDACTED]") || strings.Contains(detail, "supersecret") {
		t.Fatal(detail)
	}
}

func TestResultSummaryHidesAgentText(t *testing.T) {
	setupTestEnv(t)
	rec := store.Record{
		ID: "sum1", Status: store.Completed, Agent: "codex", Model: "gpt-5.6-terra",
		Tier: "normal", Output: "AGENT_FREE_TEXT_XYZ", Verification: "verified",
	}
	if err := runsStore().Append(rec); err != nil {
		t.Fatal(err)
	}
	summary := captureStdout(t, func() {
		if code := result([]string{"--section", "summary", "sum1"}); code != 0 {
			t.Fatal(code)
		}
	})
	if strings.Contains(summary, "AGENT_FREE_TEXT_XYZ") || !strings.Contains(summary, "dispatch.run-summary.v1") {
		t.Fatal(summary)
	}
	human := captureStdout(t, func() {
		if code := result([]string{"--json=false", "--section", "summary", "sum1"}); code != 0 {
			t.Fatal(code)
		}
	})
	if strings.Count(human, "\n") != 3 || strings.Contains(human, "AGENT_FREE_TEXT_XYZ") {
		t.Fatalf("%q", human)
	}
	agent := captureStdout(t, func() {
		if code := result([]string{"--section", "agent", "--offset", "0", "--limit", "10", "sum1"}); code != 0 {
			t.Fatal(code)
		}
	})
	if !strings.Contains(agent, "AGENT_FREE_TEXT_XYZ") {
		t.Fatal(agent)
	}
	out := captureStdout(t, func() {
		if code := result([]string{"--section", "output", "sum1"}); code != 0 {
			t.Fatal(code)
		}
	})
	if !strings.Contains(out, "AGENT_FREE_TEXT_XYZ") {
		t.Fatal(out)
	}
	files := captureStdout(t, func() {
		if code := result([]string{"--section", "files", "sum1"}); code != 0 {
			t.Fatal(code)
		}
	})
	if !strings.Contains(files, "desconhecido") {
		t.Fatal(files)
	}
	statsOut := captureStdout(t, func() {
		if code := statsCmd([]string{"--since", "all", "--json"}); code != 0 {
			t.Fatal(code)
		}
	})
	if !strings.Contains(statsOut, "dispatch.stats.v1") || !strings.Contains(statsOut, "desconhecido") {
		t.Fatal(statsOut)
	}
}

func TestVerifyInit(t *testing.T) {
	setupTestEnv(t)
	root := t.TempDir()
	t.Chdir(root)
	out := captureStdout(t, func() {
		if code := verifyCmd([]string{"--init"}); code != 0 {
			t.Fatal(code)
		}
	})
	if !strings.Contains(out, "wrote") {
		t.Fatal(out)
	}
	b, err := os.ReadFile(filepath.Join(root, ".dispatch", "verify.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != verify.Example || !strings.Contains(string(b), `"parse": "gotest"`) {
		t.Fatalf("%s", b)
	}
}

func TestProfileRoutingFiltersAndExplicitOverrides(t *testing.T) {
	h, _ := setupTestEnv(t)
	if err := os.MkdirAll(h, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "agents.json"), []byte(`{"agents":{"review":{"provider":"google","model":"gemini-pro","effort":"high","agent":"antigravity","relax":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := runner.Options{}
	if err := applyProfile(&o, "review", map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if o.Filters.Provider != "google" || o.Filters.Model != "gemini-pro" || o.Filters.Effort != "high" || o.Filters.Agent != "antigravity" || !o.Filters.Relax {
		t.Fatalf("%+v", o)
	}
	o.Filters = store.Filters{Provider: "openai", Model: "gpt-large", Effort: "low", Agent: "codex"}
	if err := applyProfile(&o, "review", map[string]bool{"provider": true, "model": true, "effort": true, "agent": true, "relax": true}); err != nil {
		t.Fatal(err)
	}
	if o.Filters.Provider != "openai" || o.Filters.Model != "gpt-large" || o.Filters.Effort != "low" || o.Filters.Agent != "codex" || o.Filters.Relax {
		t.Fatal(o.Filters)
	}
}

func TestPriorityProfileFlagAndConfig(t *testing.T) {
	h, _ := setupTestEnv(t)
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.json"), []byte(`{"priority":"speed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "agents.json"), []byte(`{"agents":{"review":{"priority":"quality","tier":"hard"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o := runner.Options{}
	if err := applyProfile(&o, "review", map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	got, err := resolveRoutingPriority(h, o.Filters.Priority, false)
	if err != nil || got != "quality" {
		t.Fatalf("profile %s %v", got, err)
	}
	if got, err = resolveRoutingPriority(h, "speed", true); err != nil || got != "speed" {
		t.Fatalf("flag %s %v", got, err)
	}
	if got, err = resolveRoutingPriority(h, "", false); err != nil || got != "speed" {
		t.Fatalf("config %s %v", got, err)
	}
	if err := os.Remove(filepath.Join(h, "config.json")); err != nil {
		t.Fatal(err)
	}
	if got, err = resolveRoutingPriority(h, "", false); err != nil || got != "balanced" {
		t.Fatalf("default %s %v", got, err)
	}
	if _, err := resolveRoutingPriority(h, "turbo", true); err == nil {
		t.Fatal("invalid flag accepted")
	}
}

func TestConfigCommandsPreserveUnknownAndEditPolicy(t *testing.T) {
	h, _ := setupTestEnv(t)
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(h, "config.json")
	if err := os.WriteFile(p, []byte(`{"future":42}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := configCmd([]string{"set", "priority", "quality"}); code != 0 {
		t.Fatal(code)
	}
	if code := configCmd([]string{"allow", "add", "providers", "openai", "google"}); code != 0 {
		t.Fatal(code)
	}
	if code := configCmd([]string{"deny", "add", "agents", "cursor"}); code != 0 {
		t.Fatal(code)
	}
	if code := configCmd([]string{"allow", "rm", "providers", "google"}); code != 0 {
		t.Fatal(code)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"future": 42`)) || !bytes.Contains(b, []byte(`"openai"`)) || bytes.Contains(b, []byte(`"google"`)) || !bytes.Contains(b, []byte(`"cursor"`)) {
		t.Fatalf("%s", b)
	}
}

func TestRouteCommandDoesNotRunCLIsOrWriteState(t *testing.T) {
	h, _ := setupTestEnv(t)
	dir := t.TempDir()
	// A binary may be installed, but even --help must not run in a route.
	marker := filepath.Join(dir, "called")
	script := "#!/bin/sh\ntouch '" + marker + "'\nexit 99\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	before, _ := os.ReadDir(h)
	var code int
	out := captureStdout(t, func() { code = runCommand([]string{"--model", "gpt-5.6-terra", "--effort", "high", "--json"}, true) })
	after, _ := os.ReadDir(h)
	if code != 0 || !strings.Contains(out, `"winner"`) || !strings.Contains(out, `"effort": "high"`) {
		t.Fatalf("%d %s", code, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("CLI invoked")
	}
	if len(before) != len(after) {
		t.Fatal("state directory changed")
	}
}
