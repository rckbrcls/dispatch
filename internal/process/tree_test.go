package process

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestCaptureAndReparentedEscalation(t *testing.T) {
	table, err := parseTable(`10 1 10 Thu Oct 15 10:00:00 2026
20 10 20 Thu Oct 15 10:00:01 2026
30 20 30 Thu Oct 15 10:00:02 2026
40 1 40 Thu Oct 15 10:00:03 2026`)
	if err != nil {
		t.Fatal(err)
	}
	tree := capture(table, 10)
	if len(tree.members) != 3 || tree.members[0].pid != 30 {
		t.Fatalf("wrong descendants: %+v", tree.members)
	}
	tree.read = func() (map[int]info, error) {
		copy := make(map[int]info)
		for pid, p := range table {
			copy[pid] = p
		}
		return copy, nil
	}
	type call struct {
		pid    int
		signal syscall.Signal
	}
	var calls []call
	tree.kill = func(pid int, signal syscall.Signal) error {
		calls = append(calls, call{pid, signal})
		if signal == syscall.SIGKILL {
			delete(table, pid)
		}
		return nil
	}
	if err := tree.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{20, 30, -10} {
		if !slices.Contains(calls, call{pid, syscall.SIGTERM}) {
			t.Fatalf("missing TERM for %d: %v", pid, calls)
		}
	}
	// The owner exits, one descendant is adopted, and another PID is reused.
	delete(table, 10)
	p := table[30]
	p.parent = 1
	table[30] = p
	p = table[20]
	p.parent = 1
	p.start = "new process"
	table[20] = p
	table[10] = info{pid: 10, parent: 1, group: 10, start: "reused group leader"}
	if err := tree.Wait(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(calls, call{30, syscall.SIGKILL}) {
		t.Fatal("reparented descendant survived", calls)
	}
	for _, pid := range []int{10, 20, 40, -10} {
		if slices.Contains(calls, call{pid, syscall.SIGKILL}) {
			t.Fatalf("signalled reused/unrelated PID %d: %v", pid, calls)
		}
	}
	if slices.Contains(calls, call{40, syscall.SIGTERM}) {
		t.Fatal("signalled unrelated process")
	}
}

func TestCaptureCommandAndOwnerIdentity(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' '10 1 10 Thu Oct 15 10:00:00 2026' '20 10 20 Thu Oct 15 10:00:01 2026' '40 1 40 Thu Oct 15 10:00:03 2026'\n"
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	tree, err := Capture(10)
	if err != nil || len(tree.members) != 2 {
		t.Fatalf("%+v %v", tree, err)
	}
	if err := tree.Include(40, "different identity"); err == nil {
		t.Fatal("accepted reused owner")
	}
	if err := tree.Include(40, "Thu Oct 15 10:00:03 2026"); err != nil {
		t.Fatal(err)
	}
	if tree.members[len(tree.members)-1].pid != 40 {
		t.Fatal("owner must stop after descendants")
	}
}

func TestInvalidProcessTable(t *testing.T) {
	for _, out := range []string{"", "10 1", "bad 1 10 Thu Oct 15 10:00:00 2026"} {
		if _, err := parseTable(out); err == nil {
			t.Fatalf("accepted %q", out)
		}
	}
}

func TestStopFinishesAfterCallerCancellation(t *testing.T) {
	table := map[int]info{10: {pid: 10, parent: 1, group: 10, start: "original"}}
	tree := capture(table, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tree.read = func() (map[int]info, error) { return table, nil }
	tree.kill = func(pid int, signal syscall.Signal) error {
		if signal == syscall.SIGTERM {
			cancel()
		}
		if signal == syscall.SIGKILL {
			delete(table, 10)
		}
		return nil
	}
	if err := tree.Stop(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if len(table) != 0 {
		t.Fatal("caller cancellation interrupted process cleanup")
	}
}
