package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestInstallCreatesEntry(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	binPath := "/usr/local/bin/dispatch"

	installed, err := IsInstalled(settingsPath)
	if err != nil {
		t.Fatalf("unexpected error checking installation: %v", err)
	}
	if installed {
		t.Fatal("expected hook to not be installed initially")
	}

	if err := Install(settingsPath, binPath); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	installed, err = IsInstalled(settingsPath)
	if err != nil {
		t.Fatalf("unexpected error checking installation: %v", err)
	}
	if !installed {
		t.Fatal("expected hook to be installed")
	}

	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings: %v", err)
	}

	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatalf("invalid json: %v", err)
	}

	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks object missing")
	}
	sessionStart, ok := hooks["SessionStart"].([]any)
	if !ok || len(sessionStart) != 1 {
		t.Fatalf("expected 1 group in SessionStart, got: %v", sessionStart)
	}
	group, ok := sessionStart[0].(map[string]any)
	if !ok {
		t.Fatal("group is not an object")
	}
	innerHooks, ok := group["hooks"].([]any)
	if !ok || len(innerHooks) != 1 {
		t.Fatalf("expected 1 hook in group, got: %v", innerHooks)
	}
	action, ok := innerHooks[0].(map[string]any)
	if !ok {
		t.Fatal("action is not an object")
	}
	if action["command"] != binPath+" context --hook" {
		t.Fatalf("unexpected command: %v", action["command"])
	}
	if action["type"] != "command" {
		t.Fatalf("unexpected type: %v", action["type"])
	}
}

func TestInstallTwiceDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	binPath := "/usr/local/bin/dispatch"

	if err := Install(settingsPath, binPath); err != nil {
		t.Fatalf("first Install failed: %v", err)
	}
	if err := Install(settingsPath, binPath); err != nil {
		t.Fatalf("second Install failed: %v", err)
	}

	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	hooks := root["hooks"].(map[string]any)
	sessionStart := hooks["SessionStart"].([]any)
	if len(sessionStart) != 1 {
		t.Fatalf("expected exactly 1 group in SessionStart after duplicate install, got %d", len(sessionStart))
	}
}

func TestInstallUpdatesMovedBinary(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")

	if err := Install(settingsPath, "/path1/dispatch"); err != nil {
		t.Fatal(err)
	}
	if err := Install(settingsPath, "/path2/dispatch"); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	sessionStart := root["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(sessionStart) != 1 {
		t.Fatalf("expected 1 group, got %d", len(sessionStart))
	}
	action := sessionStart[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if action["command"] != "/path2/dispatch context --hook" {
		t.Fatalf("command was not updated: %v", action["command"])
	}
}

func TestPreservesOtherHooksAndKeys(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")

	initialJSON := `{
  "theme": "dark",
  "tabSize": 4,
  "hooks": {
    "PreToolUse": [
      {
        "type": "command",
        "command": "echo pre_tool"
      }
    ],
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "type": "command",
            "command": "echo custom_startup",
            "timeout": 5
          }
        ]
      }
    ]
  }
}`
	if err := os.WriteFile(settingsPath, []byte(initialJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	binPath := "/usr/local/bin/dispatch"
	if err := Install(settingsPath, binPath); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		t.Fatal(err)
	}

	// Verify top-level keys preserved
	if root["theme"] != "dark" {
		t.Fatalf("theme key lost or corrupted: %v", root["theme"])
	}
	if root["tabSize"] != json.Number("4") {
		t.Fatalf("tabSize lost or changed type: %v", root["tabSize"])
	}

	// Verify other hook events preserved
	hooks := root["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Fatal("PreToolUse was lost")
	}

	// Verify SessionStart has both custom and our hook
	sessionStart := hooks["SessionStart"].([]any)
	if len(sessionStart) != 2 {
		t.Fatalf("expected 2 groups in SessionStart, got %d", len(sessionStart))
	}

	// First group should be the custom one
	firstGroup := sessionStart[0].(map[string]any)
	if firstGroup["matcher"] != "startup" {
		t.Fatalf("first group matcher corrupted: %v", firstGroup["matcher"])
	}

	// Second group should be ours
	secondGroup := sessionStart[1].(map[string]any)
	secondHooks := secondGroup["hooks"].([]any)
	secondAction := secondHooks[0].(map[string]any)
	if secondAction["command"] != binPath+" context --hook" {
		t.Fatalf("our hook command wrong: %v", secondAction["command"])
	}
}

func TestUninstallRemovesOnlyOurs(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")

	initialJSON := `{
  "theme": "dark",
  "hooks": {
    "PreToolUse": [
      {
        "type": "command",
        "command": "echo pre_tool"
      }
    ],
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "type": "command",
            "command": "echo custom_startup"
          }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/dispatch context --hook",
            "timeout": 10
          }
        ]
      }
    ]
  }
}`
	if err := os.WriteFile(settingsPath, []byte(initialJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Uninstall(settingsPath); err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}

	installed, err := IsInstalled(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if installed {
		t.Fatal("expected hook to be uninstalled")
	}

	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}

	if root["theme"] != "dark" {
		t.Fatal("theme key was lost")
	}

	hooks := root["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Fatal("PreToolUse was lost")
	}

	sessionStart := hooks["SessionStart"].([]any)
	if len(sessionStart) != 1 {
		t.Fatalf("expected 1 group remaining in SessionStart, got %d", len(sessionStart))
	}
	remainingGroup := sessionStart[0].(map[string]any)
	if remainingGroup["matcher"] != "startup" {
		t.Fatalf("custom hook group corrupted: %v", remainingGroup)
	}
}

func TestUninstallCleansEmptySessionStart(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")

	if err := Install(settingsPath, "/bin/dispatch"); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(settingsPath); err != nil {
		t.Fatal(err)
	}

	installed, err := IsInstalled(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if installed {
		t.Fatal("hook should not be installed")
	}

	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["hooks"]; ok {
		t.Fatalf("empty hooks object should have been removed: %v", root)
	}
}

func TestToggleEnabledFlag(t *testing.T) {
	dir := t.TempDir()

	if IsEnabled(dir) {
		t.Fatal("expected IsEnabled to be false initially")
	}

	if err := SetEnabled(dir, true); err != nil {
		t.Fatalf("SetEnabled(true) failed: %v", err)
	}
	if !IsEnabled(dir) {
		t.Fatal("expected IsEnabled to be true after SetEnabled(true)")
	}
	if _, err := os.Stat(filepath.Join(dir, "enabled")); err != nil {
		t.Fatalf("expected enabled flag file to exist: %v", err)
	}

	if err := SetEnabled(dir, false); err != nil {
		t.Fatalf("SetEnabled(false) failed: %v", err)
	}
	if IsEnabled(dir) {
		t.Fatal("expected IsEnabled to be false after SetEnabled(false)")
	}
	if _, err := os.Stat(filepath.Join(dir, "enabled")); !os.IsNotExist(err) {
		t.Fatal("expected enabled flag file to be deleted")
	}
}

func TestContextBlock(t *testing.T) {
	block := ContextBlock()
	if block == "" {
		t.Fatal("expected non-empty context block")
	}
	runes := utf8.RuneCountInString(block)
	if runes > 400 {
		t.Fatalf("context block too long: %d runes (> 400)", runes)
	}
	if !strings.Contains(block, "dispatch") {
		t.Fatal("context block must mention dispatch")
	}
	if !strings.Contains(block, "easy|normal|hard") {
		t.Fatal("context block must mention tiers")
	}
}

func TestSettingsPathAndDispatchHome(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tempDir, "custom-claude"))
	t.Setenv("DISPATCH_HOME", filepath.Join(tempDir, "custom-dispatch"))

	if sp := SettingsPath(""); sp != filepath.Join(tempDir, "custom-claude", "settings.json") {
		t.Fatalf("unexpected settings path: %s", sp)
	}
	if dh := DispatchHome(); dh != filepath.Join(tempDir, "custom-dispatch") {
		t.Fatalf("unexpected dispatch home: %s", dh)
	}
}

func TestHookCommandQuotesUnsafePaths(t *testing.T) {
	cases := map[string]string{
		"/home/u/.local/bin/dispatch": "/home/u/.local/bin/dispatch context --hook",
		"/Users/a b/bin/dispatch":     "'/Users/a b/bin/dispatch' context --hook",
		"/tmp/it's/dispatch":          `'/tmp/it'\''s/dispatch' context --hook`,
	}
	for in, want := range cases {
		got := HookCommand(in)
		if got != want {
			t.Errorf("HookCommand(%q) = %q, want %q", in, got, want)
		}
		if !IsHookCommand(got) {
			t.Errorf("%q not recognized as hook command", got)
		}
	}
}
