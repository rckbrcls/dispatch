package integration

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFirstWord(t *testing.T) {
	for in, want := range map[string]string{
		"/bin/dispatch context --hook":       "/bin/dispatch",
		"'/My Apps/dispatch' context --hook": "/My Apps/dispatch",
		`'/a'\''b/dispatch' context --hook`:  "/a'b/dispatch",
		"":                                   "",
	} {
		if got := firstWord(in); got != want {
			t.Errorf("firstWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnableDisableAndBrokenHook(t *testing.T) {
	uh, dh := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(uh, ".claude"))
	exe := filepath.Join(t.TempDir(), "dispatch")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if st := Inspect(dh, uh); st.Enabled || st.HookFound {
		t.Fatalf("fresh state should be off: %+v", st)
	}
	if _, err := Enable(dh, uh, exe); err != nil {
		t.Fatal(err)
	}
	st := Inspect(dh, uh)
	if !st.Enabled || !st.HookFound || st.HookProblem != "" {
		t.Fatalf("after Enable: %+v", st)
	}
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	if st := Inspect(dh, uh); st.HookProblem == "" {
		t.Fatal("hook pointing at a deleted binary must be reported as broken")
	}
	if err := Disable(dh); err != nil || Inspect(dh, uh).Enabled {
		t.Fatalf("Disable failed: %v", err)
	}
	if err := RemoveHook(uh); err != nil || Inspect(dh, uh).HookFound {
		t.Fatalf("RemoveHook failed: %v", err)
	}
}
