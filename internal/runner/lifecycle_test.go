package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"dispatch/internal/process"
	"dispatch/internal/store"
	"dispatch/internal/worktree"
)

func TestReconcile(t *testing.T) {
	var ran []string
	d := deps(t, &ran)
	ctx := context.Background()
	// Exercise identity matching deterministically even when the host sandbox blocks ps.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\necho 'Thu Oct 15 10:00:00 2026'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	start := processStart(ctx, os.Getpid())
	if start == "" {
		t.Fatal("missing identity")
	}
	wt := t.TempDir()
	file := filepath.Join(wt, "work")
	if err := os.WriteFile(file, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id       string
		pid      int
		identity string
		want     store.Status
	}{
		{"dead", 99999999, "", store.Orphaned},
		{"reused", os.Getpid(), "different start", store.Orphaned},
		{"live", os.Getpid(), start, store.Running},
		{"legacy", os.Getpid(), "", store.Pending},
		{"missing", 0, "", store.Orphaned},
	} {
		status := store.Running
		if tc.id == "legacy" {
			status = store.Pending
		}
		rec := store.Record{ID: tc.id, Status: status, PID: tc.pid, ProcStart: tc.identity, Worktree: wt}
		if err := d.Store.Append(rec); err != nil {
			t.Fatal(err)
		}
		if err := d.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := d.Store.Get(tc.id)
		if err != nil || got.Status != tc.want {
			t.Fatalf("%s: %+v %v", tc.id, got, err)
		}
		if tc.want == store.Orphaned && (got.Note == "" || got.FinishedAt.IsZero()) {
			t.Fatal(got)
		}
	}
	before, _ := os.ReadFile(d.Store.Path)
	if err := d.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(d.Store.Path)
	if string(before) != string(after) {
		t.Fatal("reconcile is not idempotent")
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "preserve" {
		t.Fatalf("work lost: %v", err)
	}
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@localhost"}, args...)...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func TestRecoverAndClean(t *testing.T) {
	var ran []string
	d := deps(t, &ran)
	root := t.TempDir()
	testGit(t, root, "init", "-q")
	testGit(t, root, "commit", "--allow-empty", "-qm", "base")
	base := testGit(t, root, "rev-parse", "HEAD")
	wt := filepath.Join(t.TempDir(), "worktree")
	branch, err := worktree.Create(root, wt, "orphan")
	if err != nil {
		t.Fatal(err)
	}
	rec := store.Record{ID: "orphan", Status: store.Orphaned, Repo: root, Worktree: wt, Branch: branch, BaseSHA: base}
	if err := d.Store.Append(rec); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(wt, "saved.txt")
	if err := os.WriteFile(file, []byte("saved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := worktree.Remove(root, wt, branch, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("clean accepted dirty orphan: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	out, stat, err := d.Recover(context.Background(), rec.ID)
	if err != nil || out.Status != store.Completed || out.Note != "recovered from orphan; unverified" || !strings.Contains(stat, "saved.txt") {
		t.Fatalf("%+v %q %v", out, stat, err)
	}
	if msg := testGit(t, wt, "log", "-1", "--format=%s"); msg != "dispatch recover orphan" {
		t.Fatal(msg)
	}
	if worktree.Dirty(wt) {
		t.Fatal("worktree still dirty")
	}
	if _, err := worktree.Merge(root, branch, rec.ID); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "saved.txt")); err != nil || string(b) != "saved\n" {
		t.Fatal("recovered work not mergeable", err)
	}
	rec.ID, rec.Status, rec.Worktree = "gone", store.Orphaned, filepath.Join(t.TempDir(), "absent")
	d.Store.Append(rec)
	before, _ := os.ReadFile(d.Store.Path)
	if _, _, err := d.Recover(context.Background(), rec.ID); err == nil {
		t.Fatal("recovered missing worktree")
	}
	after, _ := os.ReadFile(d.Store.Path)
	if string(before) != string(after) {
		t.Fatal("missing worktree modified record")
	}
}

func TestCancelWaitsForProcesses(t *testing.T) {
	requireProcessTree(t)
	var ran []string
	d := deps(t, &ran)
	owner := exec.Command("sleep", "60")
	agent := exec.Command("sh", "-c", `trap '' TERM; sleep 60 & wait`)
	agent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer owner.Process.Kill()
	go owner.Wait()
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-agent.Process.Pid, syscall.SIGKILL)
	go agent.Wait()
	rec := store.Record{ID: "cancel", Status: store.Running, PID: owner.Process.Pid, AgentPID: agent.Process.Pid, ProcStart: processStart(context.Background(), owner.Process.Pid)}
	if err := d.Store.Append(rec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	out, err := d.Cancel(ctx, rec.ID)
	if err != nil || out.Status != store.Cancelled || alive(-rec.AgentPID) || alive(rec.PID) {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestReconcileStopsOrphanGroup(t *testing.T) {
	requireProcessTree(t)
	var ran []string
	d := deps(t, &ran)
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	rec := store.Record{ID: "orphan-group", Status: store.Pending, PID: 99999999, AgentPID: cmd.Process.Pid, Worktree: t.TempDir()}
	if err := d.Store.Append(rec); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("orphan group did not receive TERM")
	}
	got, err := d.Store.Get(rec.ID)
	if err != nil || got.Status != store.Orphaned {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := os.Stat(rec.Worktree); err != nil {
		t.Fatal("worktree removed", err)
	}
}

func TestWaitTimeoutPreservesRecord(t *testing.T) {
	var ran []string
	d := deps(t, &ran)
	rec := store.Record{ID: "live-wait", Status: store.Running, PID: os.Getpid()}
	if err := d.Store.Append(rec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := d.Wait(ctx, rec.ID, time.Millisecond)
	if err != context.Canceled || got.ID != rec.ID {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestDetachedDescendantsStopped(t *testing.T) {
	requireProcessTree(t)
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("perl unavailable for detached process fixture")
	}
	for _, orphan := range []bool{false, true} {
		name := "cancel"
		if orphan {
			name = "reconcile"
		}
		t.Run(name, func(t *testing.T) {
			var ran []string
			d := deps(t, &ran)
			path := filepath.Join(t.TempDir(), "child")
			program := `use POSIX qw(setsid); setsid() >= 0 or die $!; $SIG{TERM}="IGNORE"; open(my $f,">",$ARGV[0]) or die $!; print $f "$$\n"; close($f); exec "sleep","60";`
			agent := exec.Command("sh", "-c", `"$1" -e "$2" "$3" & trap 'exit' TERM; wait`, "sh", perl, program, path)
			agent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := agent.Start(); err != nil {
				t.Fatal(err)
			}
			defer syscall.Kill(-agent.Process.Pid, syscall.SIGKILL)
			done := make(chan error, 1)
			go func() { done <- agent.Wait() }()
			var child int
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				b, _ := os.ReadFile(path)
				child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
				if child > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if child <= 0 {
				t.Fatal("detached process did not start")
			}
			defer syscall.Kill(child, syscall.SIGKILL)
			group, err := syscall.Getpgid(child)
			if err != nil || group == agent.Process.Pid {
				t.Fatalf("process did not detach: %d %v", group, err)
			}
			ownerPID := 99999999
			if !orphan {
				owner := exec.Command("sleep", "60")
				if err := owner.Start(); err != nil {
					t.Fatal(err)
				}
				defer owner.Process.Kill()
				go owner.Wait()
				ownerPID = owner.Process.Pid
			}
			rec := store.Record{ID: name, Status: store.Running, PID: ownerPID, AgentPID: agent.Process.Pid, ProcStart: processStart(context.Background(), ownerPID)}
			if err := d.Store.Append(rec); err != nil {
				t.Fatal(err)
			}
			want := store.Cancelled
			if orphan {
				if err := d.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
				want = store.Orphaned
			} else {
				if _, err := d.Cancel(context.Background(), rec.ID); err != nil {
					t.Fatal(err)
				}
			}
			got, err := d.Store.Get(rec.ID)
			if err != nil || got.Status != want {
				t.Fatalf("%+v %v", got, err)
			}
			if err := syscall.Kill(child, 0); err != syscall.ESRCH {
				t.Fatalf("escaped descendant %d survived: %v", child, err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("agent survived")
			}
		})
	}
}

func requireProcessTree(t *testing.T) {
	t.Helper()
	if _, err := process.Capture(os.Getpid()); err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("sandbox blocks process table: %v", err)
		}
		t.Fatal(err)
	}
}
