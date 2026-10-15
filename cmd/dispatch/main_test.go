package main

import (
	"bytes"
	"io"
	"os"
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
