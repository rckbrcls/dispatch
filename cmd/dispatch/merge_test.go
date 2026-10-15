package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dispatch/internal/store"
	"dispatch/internal/worktree"
)

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	outC := make(chan string)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		outC <- buf.String()
	}()

	fn()

	w.Close()
	os.Stderr = old
	return <-outC
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@localhost"}, args...)...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func TestMergeClean(t *testing.T) {
	setupTestEnv(t)
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(root, "init.txt"), []byte("init\n"), 0o644)
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-qm", "init")

	wt := filepath.Join(t.TempDir(), "wt1")
	branch, err := worktree.Create(root, wt, "run-clean")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("feature content\n"), 0o644)
	gitRun(t, wt, "add", "-A")
	gitRun(t, wt, "commit", "-qm", "feature commit")

	rec := store.Record{
		ID:        "run-clean",
		Status:    store.Completed,
		Repo:      root,
		Worktree:  wt,
		Branch:    branch,
		CreatedAt: time.Now(),
	}
	if err := runsStore().Append(rec); err != nil {
		t.Fatal(err)
	}

	var code int
	out := captureStdout(t, func() {
		code = merge([]string{"run-clean"})
	})
	if code != 0 {
		t.Fatalf("merge returned code %d, output: %s", code, out)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("unmarshal json: %v, out: %s", err, out)
	}
	if body["id"] != "run-clean" || body["merged"] != branch || body["cleaned"] != true {
		t.Fatalf("unexpected body: %#v", body)
	}
	if _, err := os.Stat(filepath.Join(root, "feature.txt")); err != nil {
		t.Fatalf("feature.txt missing in root: %v", err)
	}
}

func TestMergeConflictTwoRunsEditingSameFile(t *testing.T) {
	setupTestEnv(t)
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(root, "shared.txt"), []byte("base line\n"), 0o644)
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-qm", "base commit")

	// Run 1
	wt1 := filepath.Join(t.TempDir(), "wt1")
	b1, err := worktree.Create(root, wt1, "r1")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt1, "shared.txt"), []byte("run 1 line\n"), 0o644)
	gitRun(t, wt1, "add", "-A")
	gitRun(t, wt1, "commit", "-qm", "r1 edit")
	rec1 := store.Record{
		ID: "r1", Status: store.Completed, Repo: root, Worktree: wt1, Branch: b1, CreatedAt: time.Now(),
	}
	if err := runsStore().Append(rec1); err != nil {
		t.Fatal(err)
	}

	// Run 2 from the same base
	wt2 := filepath.Join(t.TempDir(), "wt2")
	b2, err := worktree.Create(root, wt2, "r2")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt2, "shared.txt"), []byte("run 2 conflicting line\n"), 0o644)
	gitRun(t, wt2, "add", "-A")
	gitRun(t, wt2, "commit", "-qm", "r2 edit")
	rec2 := store.Record{
		ID: "r2", Status: store.Completed, Repo: root, Worktree: wt2, Branch: b2, CreatedAt: time.Now(),
	}
	if err := runsStore().Append(rec2); err != nil {
		t.Fatal(err)
	}

	// Merge Run 1 successfully
	var code1 int
	captureStdout(t, func() {
		code1 = merge([]string{"r1"})
	})
	if code1 != 0 {
		t.Fatalf("merge r1 failed: code %d", code1)
	}

	// Now merge Run 2: must detect conflict before touching working tree!
	var code2 int
	out2 := captureStdout(t, func() {
		code2 = merge([]string{"r2"})
	})
	if code2 != 1 {
		t.Fatalf("expected merge r2 to return code 1, got %d, out: %s", code2, out2)
	}

	var body2 map[string]any
	if err := json.Unmarshal([]byte(out2), &body2); err != nil {
		t.Fatalf("unmarshal json: %v, out: %s", err, out2)
	}
	if body2["status"] != "merge_conflict" {
		t.Fatalf("expected status merge_conflict, got %#v", body2["status"])
	}
	files, ok := body2["conflicts"].([]any)
	if !ok || len(files) == 0 || files[0] != "shared.txt" {
		t.Fatalf("expected files to contain shared.txt, got %#v", body2["conflicts"])
	}
	hint, _ := body2["hint"].(string)
	if !strings.Contains(hint, "dispatch run --resume r2") {
		t.Fatalf("hint should mention resume: %s", hint)
	}

	// Verify working tree and index in root are completely clean!
	status := gitRun(t, root, "status", "--porcelain")
	if status != "" {
		t.Fatalf("root working tree/index was touched! git status:\n%s", status)
	}

	// Verify record in store has exitClass == "merge_conflict"
	stored, err := runsStore().Get("r2")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ExitClass != "merge_conflict" {
		t.Fatalf("expected stored ExitClass merge_conflict, got %q", stored.ExitClass)
	}

	// Verify --force does NOT bypass merge conflict!
	var codeForce int
	outForce := captureStdout(t, func() {
		codeForce = merge([]string{"--force", "r2"})
	})
	if codeForce != 1 {
		t.Fatalf("expected merge --force r2 to fail with 1, got %d", codeForce)
	}
	var bodyForce map[string]any
	if err := json.Unmarshal([]byte(outForce), &bodyForce); err != nil {
		t.Fatal(err)
	}
	if bodyForce["status"] != "merge_conflict" {
		t.Fatalf("expected status merge_conflict even with --force, got %#v", bodyForce["status"])
	}
}

func TestMergeConcurrentLock(t *testing.T) {
	setupTestEnv(t)
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	gitRun(t, root, "commit", "--allow-empty", "-qm", "init")

	wt := filepath.Join(t.TempDir(), "wt")
	branch, err := worktree.Create(root, wt, "r-lock")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o644)
	gitRun(t, wt, "add", "-A")
	gitRun(t, wt, "commit", "-qm", "a")

	rec := store.Record{
		ID: "r-lock", Status: store.Completed, Repo: root, Worktree: wt, Branch: branch, CreatedAt: time.Now(),
	}
	if err := runsStore().Append(rec); err != nil {
		t.Fatal(err)
	}

	// Lower timeout for fast test
	oldTimeout := worktree.LockTimeout
	worktree.LockTimeout = 100 * time.Millisecond
	defer func() { worktree.LockTimeout = oldTimeout }()

	// Acquire lock beforehand to simulate another merge in progress
	unlock, err := worktree.LockMerge(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	var code int
	stderr := captureStderr(t, func() {
		code = merge([]string{"r-lock"})
	})
	if code != 1 {
		t.Fatalf("expected merge to fail with code 1, got %d", code)
	}
	if !strings.Contains(stderr, "timeout waiting for lock") {
		t.Fatalf("expected lock timeout error in stderr, got: %s", stderr)
	}
}

func TestMergePostMergeVerificationFailure(t *testing.T) {
	setupTestEnv(t)
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(root, "base.txt"), []byte("base\n"), 0o644)
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-qm", "base commit")

	// Set up .dispatch/verify.json with a check that checks for absence of "broken.txt"
	vDir := filepath.Join(root, ".dispatch")
	if err := os.MkdirAll(vDir, 0o755); err != nil {
		t.Fatal(err)
	}
	verifyConfig := `{
		"checks": [
			{
				"name": "no-broken-check",
				"argv": ["test", "!", "-f", "broken.txt"],
				"required": true
			}
		]
	}`
	os.WriteFile(filepath.Join(vDir, "verify.json"), []byte(verifyConfig), 0o644)
	gitRun(t, root, "add", ".dispatch/verify.json")
	gitRun(t, root, "commit", "-qm", "add verify.json")

	// Create run that introduces broken.txt
	wt := filepath.Join(t.TempDir(), "wt")
	branch, err := worktree.Create(root, wt, "r-vfail")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "broken.txt"), []byte("break\n"), 0o644)
	gitRun(t, wt, "add", "-A")
	gitRun(t, wt, "commit", "-qm", "add broken.txt")

	rec := store.Record{
		ID: "r-vfail", Status: store.Completed, Repo: root, Worktree: wt, Branch: branch, CreatedAt: time.Now(),
	}
	if err := runsStore().Append(rec); err != nil {
		t.Fatal(err)
	}

	var code int
	out := captureStdout(t, func() {
		code = merge([]string{"r-vfail"})
	})
	if code != 1 {
		t.Fatalf("expected post-merge failure to return code 1, got %d, out: %s", code, out)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("unmarshal json: %v, out: %s", err, out)
	}
	if body["post_merge_verification"] != "failed" {
		t.Fatalf("expected post_merge_verification: failed, got %#v", body["post_merge_verification"])
	}
	revertCmd, ok := body["revert"].(string)
	if !ok || !strings.HasPrefix(revertCmd, "git revert -m 1 ") {
		t.Fatalf("expected revert command starting with git revert -m 1, got %#v", body["revert"])
	}

	// Verify merge commit was NOT undone! broken.txt is in root!
	if _, err := os.Stat(filepath.Join(root, "broken.txt")); err != nil {
		t.Fatalf("merge was undone: broken.txt does not exist in root: %v", err)
	}

	// Test --no-verify skips check
	wt2 := filepath.Join(t.TempDir(), "wt2")
	branch2, err := worktree.Create(root, wt2, "r-noverify")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt2, "ok.txt"), []byte("ok\n"), 0o644)
	gitRun(t, wt2, "add", "-A")
	gitRun(t, wt2, "commit", "-qm", "add ok.txt")

	rec2 := store.Record{
		ID: "r-noverify", Status: store.Completed, Repo: root, Worktree: wt2, Branch: branch2, CreatedAt: time.Now(),
	}
	if err := runsStore().Append(rec2); err != nil {
		t.Fatal(err)
	}

	var codeNoVerify int
	outNoVerify := captureStdout(t, func() {
		codeNoVerify = merge([]string{"--no-verify", "r-noverify"})
	})
	if codeNoVerify != 0 {
		t.Fatalf("expected --no-verify merge to succeed with 0, got %d, out: %s", codeNoVerify, outNoVerify)
	}
	var bodyNoVerify map[string]any
	if err := json.Unmarshal([]byte(outNoVerify), &bodyNoVerify); err != nil {
		t.Fatal(err)
	}
	if bodyNoVerify["post_merge_verification"] == "failed" {
		t.Fatalf("post_merge_verification should not be failed with --no-verify")
	}
}

func TestMergeGitWithoutMergeTree(t *testing.T) {
	setupTestEnv(t)
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	gitRun(t, root, "commit", "--allow-empty", "-qm", "init")

	wt := filepath.Join(t.TempDir(), "wt")
	branch, err := worktree.Create(root, wt, "r-oldgit")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "old.txt"), []byte("old\n"), 0o644)
	gitRun(t, wt, "add", "-A")
	gitRun(t, wt, "commit", "-qm", "old commit")

	rec := store.Record{
		ID: "r-oldgit", Status: store.Completed, Repo: root, Worktree: wt, Branch: branch, CreatedAt: time.Now(),
	}
	if err := runsStore().Append(rec); err != nil {
		t.Fatal(err)
	}

	// Simulate older git via injectable function
	oldSupports := worktree.SupportsMergeTree
	worktree.SupportsMergeTree = func(root string) bool { return false }
	defer func() { worktree.SupportsMergeTree = oldSupports }()

	var code int
	var stderr string
	out := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			code = merge([]string{"r-oldgit"})
		})
	})
	if code != 0 {
		t.Fatalf("merge returned %d, out: %s, stderr: %s", code, out, stderr)
	}
	if !strings.Contains(stderr, "does not support merge-tree") {
		t.Fatalf("expected warning in stderr, got: %s", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "old.txt")); err != nil {
		t.Fatalf("merge did not bring old.txt: %v", err)
	}
}

func TestMergeRefusesUncommittedChangesOverlap(t *testing.T) {
	setupTestEnv(t)
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(root, "edit.txt"), []byte("original\n"), 0o644)
	os.WriteFile(filepath.Join(root, "other.txt"), []byte("other\n"), 0o644)
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-qm", "init")

	wt := filepath.Join(t.TempDir(), "wt")
	branch, err := worktree.Create(root, wt, "r-uncommitted")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "edit.txt"), []byte("branch change\n"), 0o644)
	gitRun(t, wt, "add", "-A")
	gitRun(t, wt, "commit", "-qm", "branch edit")

	rec := store.Record{
		ID: "r-uncommitted", Status: store.Completed, Repo: root, Worktree: wt, Branch: branch, CreatedAt: time.Now(),
	}
	if err := runsStore().Append(rec); err != nil {
		t.Fatal(err)
	}

	// Target working tree has uncommitted change in edit.txt
	os.WriteFile(filepath.Join(root, "edit.txt"), []byte("uncommitted change in root\n"), 0o644)

	var code int
	stderr := captureStderr(t, func() {
		code = merge([]string{"r-uncommitted"})
	})
	if code != 1 {
		t.Fatalf("expected merge to refuse with code 1, got %d", code)
	}
	if !strings.Contains(stderr, "working tree has uncommitted changes in files touched by") || !strings.Contains(stderr, "edit.txt") {
		t.Fatalf("expected uncommitted changes error message, got: %s", stderr)
	}

	// Verify target file still has uncommitted change
	b, _ := os.ReadFile(filepath.Join(root, "edit.txt"))
	if string(b) != "uncommitted change in root\n" {
		t.Fatalf("working tree file was overwritten: %s", string(b))
	}
}
