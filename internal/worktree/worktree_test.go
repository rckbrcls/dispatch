package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestNumStat(t *testing.T) {
	root := t.TempDir()
	run(t, root, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o644)
	run(t, root, "add", "-A")
	run(t, root, "commit", "-qm", "base")
	base := strings.TrimSpace(string(mustGit(t, root, "rev-parse", "HEAD")))
	files, ins, del, known, err := NumStat(root, base)
	if err != nil || !known || files != 0 || ins != 0 || del != 0 {
		t.Fatalf("empty %d %d %d %v %v", files, ins, del, known, err)
	}
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("b\n"), 0o644)
	run(t, root, "add", "b.txt")
	files, ins, del, known, err = NumStat(root, base)
	if err != nil || !known || files != 1 || ins != 1 || del != 0 {
		t.Fatalf("added %d %d %d %v %v", files, ins, del, known, err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseGitVersion(t *testing.T) {
	cases := []struct {
		input     string
		wantMaj   int
		wantMin   int
		wantOk    bool
		supported bool
	}{
		{"git version 2.55.0", 2, 55, true, true},
		{"git version 2.38.0", 2, 38, true, true},
		{"git version 2.38.1.windows.1", 2, 38, true, true},
		{"git version 2.37.1 (Apple Git-137.1)", 2, 37, true, false},
		{"git version 1.9.5", 1, 9, true, false},
		{"git version 3.0.0", 3, 0, true, true},
		{"invalid", 0, 0, false, false},
	}
	for _, tc := range cases {
		maj, min, ok := parseGitVersion(tc.input)
		if ok != tc.wantOk || maj != tc.wantMaj || min != tc.wantMin {
			t.Errorf("parseGitVersion(%q) = (%d, %d, %v), want (%d, %d, %v)", tc.input, maj, min, ok, tc.wantMaj, tc.wantMin, tc.wantOk)
		}
		supp := ok && (maj > 2 || (maj == 2 && min >= 38))
		if supp != tc.supported {
			t.Errorf("supported(%q) = %v, want %v", tc.input, supp, tc.supported)
		}
	}
}

func TestLockMergeStaleDetection(t *testing.T) {
	root := t.TempDir()
	run(t, root, "init", "-q", "-b", "main")
	run(t, root, "commit", "--allow-empty", "-qm", "init")

	unlock1, err := LockMerge(root, "main")
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	// Holding lock, second lock with short timeout should fail
	oldTimeout := LockTimeout
	LockTimeout = 50 * time.Millisecond
	defer func() { LockTimeout = oldTimeout }()

	_, err = LockMerge(root, "main")
	if err == nil {
		t.Fatal("expected error acquiring held lock")
	}
	if !strings.Contains(err.Error(), "timeout waiting for lock") {
		t.Fatalf("unexpected error message: %v", err)
	}

	unlock1()

	// Now lock should succeed
	unlock2, err := LockMerge(root, "main")
	if err != nil {
		t.Fatalf("second lock after unlock: %v", err)
	}
	unlock2()
}
