package adapter

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
)

func TestAgentCancellationKillsGrandchild(t *testing.T) {
	requireProcessTree(t)
	for _, stubborn := range []bool{false, true} {
		t.Run(strconv.FormatBool(stubborn), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pid")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// The intermediate shell reaps sleep before exiting on TERM.
			script := `sh -c 'trap "wait; exit" TERM; sleep 60 & echo $! > "$1"; wait' sh "$1" & trap 'wait; exit' TERM; wait`
			if stubborn {
				script = `trap '' TERM; sh -c 'sleep 60 & echo $! > "$1"; wait' sh "$1" & wait`
			}
			cmd := exec.CommandContext(ctx, "sh", "-c", script, "sh", path)
			done := make(chan error, 1)
			go func() { done <- runAgent(cmd, nil) }()
			var pid int
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				b, _ := os.ReadFile(path)
				pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
				if pid > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if pid <= 0 {
				cancel()
				<-done
				t.Fatal("grandchild did not start")
			}
			defer syscall.Kill(pid, syscall.SIGKILL)
			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatal(err)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(12 * time.Second):
				t.Fatal("cancellation hung")
			}
			deadline = time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) && syscall.Kill(pid, 0) == nil {
				time.Sleep(20 * time.Millisecond)
			}
			if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Fatalf("grandchild %d survives: %v", pid, err)
			}
		})
	}
}

func TestAgentCancellationKillsDetachedGrandchild(t *testing.T) {
	requireProcessTree(t)
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("perl unavailable for detached process fixture")
	}
	for _, timeout := range []bool{false, true} {
		t.Run("timeout="+strconv.FormatBool(timeout), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "detached-pid")
			// Ignore TERM in a new session to exercise identity checks after adoption.
			program := `use POSIX qw(setsid); setsid() >= 0 or die $!; $SIG{TERM}="IGNORE"; open(my $f,">",$ARGV[0]) or die $!; print $f "$$\n"; close($f); exec "sleep","60";`
			ctx, cancel := context.WithCancel(context.Background())
			if timeout {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			}
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", `"$1" -e "$2" "$3" & trap 'exit' TERM; wait`, "sh", perl, program, path)
			done := make(chan error, 1)
			go func() { done <- runAgent(cmd, nil) }()
			var pid int
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				b, _ := os.ReadFile(path)
				pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
				if pid > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if pid <= 0 {
				cancel()
				<-done
				t.Fatal("detached grandchild did not start")
			}
			defer syscall.Kill(pid, syscall.SIGKILL)
			group, err := syscall.Getpgid(pid)
			if err != nil || group != pid { // setsid makes it a group leader; cmd.Process would race with Start
				t.Fatalf("grandchild did not escape group: %d %v", group, err)
			}
			if !timeout {
				cancel()
			}
			select {
			case err := <-done:
				if errors.Is(err, ErrProcessesAlive) {
					t.Fatal(err)
				}
			case <-time.After(13 * time.Second):
				t.Fatal("cancellation hung")
			}
			deadline = time.Now().Add(2 * time.Second)
			for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Fatalf("detached grandchild %d survived: %v", pid, err)
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
