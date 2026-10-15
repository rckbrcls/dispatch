package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstall(t *testing.T) {
	dir := t.TempDir()
	p, err := Install(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), "---\nname: dispatch\n") {
		t.Fatalf("bad frontmatter: %q", b[:40])
	}
	if _, err := Install(dir, false); err == nil {
		t.Fatal("second install must refuse without force")
	}
	if _, err := Install(dir, true); err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(dir, "dispatch", "SKILL.md") {
		t.Fatal(p)
	}
}
