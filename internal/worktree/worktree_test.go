package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestLifecycle(t *testing.T) {
	root := t.TempDir()
	run(t, root, "init", "-q")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	run(t, root, "add", "-A")
	run(t, root, "commit", "-q", "-m", "init")

	resolved, _ := filepath.EvalSymlinks(root)
	if got := Root(root); got != resolved {
		t.Fatalf("Root = %q want %q", got, resolved)
	}
	if !HasCommits(root) || Dirty(root) {
		t.Fatal("expected clean repo with commits")
	}

	if err := Exclude(root, ".dispatch/"); err != nil {
		t.Fatal(err)
	}
	Exclude(root, ".dispatch/") // idempotent
	b, _ := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if n := strings.Count(string(b), ".dispatch/"); n != 1 {
		t.Fatalf("exclude entries = %d", n)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	branch, err := Create(root, wt, "r1")
	if err != nil || branch != "dispatch/r1" {
		t.Fatalf("%q %v", branch, err)
	}
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("b"), 0o644)
	if ok, err := Commit(wt, "r1"); !ok || err != nil {
		t.Fatalf("commit: %v %v", ok, err)
	}
	if ok, _ := Commit(wt, "r1"); ok {
		t.Fatal("nothing left to commit")
	}
	if _, err := Merge(root, branch, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); err != nil {
		t.Fatal("merge did not bring b.txt")
	}
	if err := Remove(root, wt, branch, false); err != nil {
		t.Fatal(err)
	}
}
