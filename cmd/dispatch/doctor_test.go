package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/store"
)

func TestDoctorExitZero(t *testing.T) {
	setupTestEnv(t)
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	out := captureStdout(t, func() {
		if code := doctorCmd(nil); code != 0 {
			t.Fatalf("exit %d", code)
		}
	})
	if !strings.Contains(out, "no installed agent CLIs") {
		t.Fatalf("%q", out)
	}

	dir := t.TempDir()
	installGoldenCLIs(t, dir)
	t.Setenv("PATH", dir)
	var code int
	out = captureStdout(t, func() {
		code = doctorCmd([]string{"--json"})
	})
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	var reports []adapter.ProbeReport
	if err := json.Unmarshal([]byte(out), &reports); err != nil {
		t.Fatal(err, out)
	}
	if len(reports) != 4 {
		t.Fatalf("%d reports: %s", len(reports), out)
	}
	for _, r := range reports {
		if r.Status != adapter.StatusOK || r.Version == "" {
			t.Fatalf("%+v", r)
		}
	}

	helpPath := filepath.Join(dir, "codex.help")
	raw, err := os.ReadFile(helpPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helpPath, []byte(strings.ReplaceAll(string(raw), "--json", "--json-removed")), 0o644); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() {
		code = doctorCmd(nil)
	})
	if code != 0 || !strings.Contains(out, "DEGRADED") || !strings.Contains(out, "--json") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestAgentsShowsQuotaSource(t *testing.T) {
	home, _ := setupTestEnv(t)
	t.Setenv("PATH", t.TempDir())
	out := captureStdout(t, func() { agents(nil) })
	if !strings.Contains(out, "source=codex app-server confidence=official") {
		t.Fatalf("%s", out)
	}
	if !strings.Contains(out, "source=agy /usage confidence=official") {
		t.Fatalf("%s", out)
	}
	if !strings.Contains(out, "source=empirical confidence=empirical risk=unknown used=unknown") {
		t.Fatalf("%s", out)
	}
	if strings.Contains(out, "used=0") || strings.Contains(out, "%") {
		t.Fatalf("invented percent:\n%s", out)
	}

	st := store.Store{Path: filepath.Join(home, "runs.jsonl")}
	now := time.Now()
	if err := st.Append(store.Record{
		ID: "1", Status: store.Failed, Agent: "cursor", Error: "429 too many requests",
		CreatedAt: now, FinishedAt: now,
		Attempts: []store.Attempt{{Agent: "cursor", ExitClass: "rate_limit", Error: "429 too many requests"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCooldown("grok", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	raw := captureStdout(t, func() { agents([]string{"--json"}) })
	var infos []agentInfo
	if err := json.Unmarshal([]byte(raw), &infos); err != nil {
		t.Fatal(err, raw)
	}
	by := map[string]agentInfo{}
	for _, info := range infos {
		by[info.Name] = info
		if info.Quota.Confidence == adapter.ConfidenceEmpirical {
			b, _ := json.Marshal(info.Quota)
			if strings.Contains(string(b), `"used"`) {
				t.Fatalf("%s invented used: %s", info.Name, b)
			}
		}
	}
	if by["codex"].Quota.Source != adapter.SourceCodexAppServer || by["codex"].Quota.Confidence != adapter.ConfidenceOfficial {
		t.Fatalf("%+v", by["codex"].Quota)
	}
	if by["cursor"].Quota.Risk != "recent_rate_limit" || by["cursor"].Quota.Confidence != adapter.ConfidenceEmpirical {
		t.Fatalf("%+v", by["cursor"].Quota)
	}
	if by["grok"].Quota.Risk != "cooldown" || len(by["grok"].Quota.Samples) != 1 || by["grok"].Quota.Samples[0].Used != nil {
		t.Fatalf("%+v", by["grok"].Quota)
	}
}

func installGoldenCLIs(t *testing.T, dir string) {
	t.Helper()
	root := filepath.Join("..", "..", "internal", "adapter", "testdata")
	bins := map[string]string{"codex": "codex", "cursor": "cursor-agent", "grok": "grok", "antigravity": "agy"}
	for name, bin := range bins {
		versions, err := os.ReadDir(filepath.Join(root, name))
		if err != nil || len(versions) == 0 {
			t.Fatal(name, err)
		}
		base := filepath.Join(root, name, versions[0].Name())
		ver, err := os.ReadFile(filepath.Join(base, "version.txt"))
		if err != nil {
			t.Fatal(err)
		}
		help, err := os.ReadFile(filepath.Join(base, "help.txt"))
		if err != nil {
			t.Fatal(err)
		}
		verPath := filepath.Join(dir, bin+".version")
		helpPath := filepath.Join(dir, bin+".help")
		if err := os.WriteFile(verPath, ver, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(helpPath, help, 0o644); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\ncase \"$*\" in\n--version) /bin/cat '" + verPath + "' ;;\n--help|\"exec --help\") /bin/cat '" + helpPath + "' ;;\n*) echo refusing >&2; exit 97 ;;\nesac\n"
		if err := os.WriteFile(filepath.Join(dir, bin), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}
