package adapter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAgentCancellationKillsGrandchild(t *testing.T) {
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
