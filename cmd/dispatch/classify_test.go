package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dispatch/internal/adapter"
	"dispatch/internal/classify"
)

func TestClassifyCommandOffline(t *testing.T) {
	h, _ := setupTestEnv(t)
	t.Setenv("PATH", t.TempDir()) // no CLI can run, including git
	var code int
	out := captureStdout(t, func() { code = classifyCmd([]string{"fix auth", "--tier", "easy", "--json", "--explain"}) })
	var p classify.Prediction
	if err := json.Unmarshal([]byte(out), &p); err != nil || code != 0 || p.SuggestedTier != adapter.TierNormal || p.Score < classify.Threshold || len(p.Reasons) != 24 {
		t.Fatalf("%d %s %v", code, out, err)
	}
	if _, err := os.Stat(filepath.Join(h, "classifier.json")); !os.IsNotExist(err) {
		t.Fatal("inference wrote state")
	}
	out = captureStdout(t, func() { code = classifyCmd([]string{"fix typo", "--tier", "easy"}) })
	if code != 0 || !strings.Contains(out, "tier=easy") || !strings.Contains(out, "bias") {
		t.Fatalf("%d %s", code, out)
	}
	out = captureStdout(t, func() { code = classifyCmd([]string{"--reset", "--json"}) })
	if code != 0 || !strings.Contains(out, `"reset": true`) {
		t.Fatalf("%d %s", code, out)
	}
	m, err := classify.Load(filepath.Join(h, "classifier.json"))
	if err != nil || m.Tiers[adapter.TierEasy].Samples != 0 {
		t.Fatalf("%+v %v", m, err)
	}
	for _, args := range [][]string{{}, {"fix", "--tier", "huge"}, {"fix", "--unknown"}, {"fix", "--tier"}} {
		if code := classifyCmd(args); code == 0 {
			t.Fatalf("accepted %v", args)
		}
	}
}
