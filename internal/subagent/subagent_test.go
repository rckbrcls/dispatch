package subagent

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAllProjectWins(t *testing.T) {
	d := t.TempDir()
	user, proj := filepath.Join(d, "u.json"), ProjectPath(d)
	write(t, user, `{"agents":{"a":{"tier":"easy"},"b":{"tier":"hard"}}}`)
	write(t, proj, `{"agents":{"a":{"tier":"normal"}}}`)
	m, err := LoadAll(user, proj)
	if err != nil {
		t.Fatal(err)
	}
	if m["a"].Tier != "normal" || m["b"].Tier != "hard" {
		t.Fatalf("merge = %+v", m)
	}
	if m, _ := LoadAll(filepath.Join(d, "none"), filepath.Join(d, "none2")); len(m) != 0 {
		t.Fatalf("missing files = %+v", m)
	}
}

func TestValidate(t *testing.T) {
	known := map[string]bool{"codex": true, "grok": true}
	ok := Profile{Tier: "hard", Prefer: []string{"codex"}, Avoid: []string{"grok"}, Timeout: "5m"}
	if p := Validate("r", ok, known); len(p) != 0 {
		t.Fatalf("unexpected %v", p)
	}
	bad := Profile{Tier: "x", Prefer: []string{"nope", "grok"}, Avoid: []string{"grok"}, Timeout: "zz"}
	if p := Validate("r", bad, known); len(p) != 4 {
		t.Fatalf("problems = %v", p)
	}
}
