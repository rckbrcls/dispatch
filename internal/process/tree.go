// Package process stops agent descendants even when they leave the agent's group.
package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type info struct {
	pid, parent, group int
	start              string
}

type Tree struct {
	group   int
	members []info
	table   map[int]info
	read    func() (map[int]info, error)
	kill    func(int, syscall.Signal) error
}

func readTable() (map[int]info, error) {
	// One snapshot associates ancestry with start times before any parent exits.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,pgid=,lstart=").Output()
	if err != nil {
		return nil, fmt.Errorf("read process tree: %w", err)
	}
	return parseTable(string(out))
}

func parseTable(out string) (map[int]info, error) {
	table := make(map[int]info)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			return nil, fmt.Errorf("invalid ps process row %q", line)
		}
		pid, e1 := strconv.Atoi(fields[0])
		parent, e2 := strconv.Atoi(fields[1])
		group, e3 := strconv.Atoi(fields[2])
		if e1 != nil || e2 != nil || e3 != nil || pid < 0 {
			return nil, fmt.Errorf("invalid ps process row %q", line)
		}
		table[pid] = info{pid: pid, parent: parent, group: group, start: strings.Join(fields[3:], " ")}
	}
	return table, nil
}

// Capture retains identities so reparented descendants can still be stopped.
func Capture(leader int) (*Tree, error) {
	table, err := readTable()
	if err != nil {
		return nil, err
	}
	return capture(table, leader), nil
}

func capture(table map[int]info, leader int) *Tree {
	t := &Tree{group: leader, table: table, read: readTable, kill: syscall.Kill}
	children := make(map[int][]int)
	for pid, p := range table {
		children[p.parent] = append(children[p.parent], pid)
	}
	seen := make(map[int]bool)
	var visit func(int)
	visit = func(pid int) {
		if seen[pid] {
			return
		}
		seen[pid] = true
		for _, child := range children[pid] {
			visit(child)
		}
		if p, ok := table[pid]; ok {
			t.members = append(t.members, p)
		}
	}
	if leader > 0 {
		visit(leader)
		// The leader may already be gone, leaving members of its original group.
		for pid, p := range table {
			if p.group == leader {
				visit(pid)
			}
		}
	}
	return t
}

// Include adds the dispatch owner last, after validating its recorded identity.
func (t *Tree) Include(pid int, start string) error {
	if pid <= 0 {
		return nil
	}
	p, ok := t.table[pid]
	if !ok {
		return nil
	}
	if start != "" && p.start != strings.Join(strings.Fields(start), " ") {
		return fmt.Errorf("process %d identity changed", pid)
	}
	for _, member := range t.members {
		if member.pid == pid {
			return nil
		}
	}
	t.members = append(t.members, p)
	return nil
}

func same(p info, table map[int]info) bool {
	current, ok := table[p.pid]
	return ok && current.start == p.start
}

// Signal rechecks each captured identity, including after adoption by init/launchd.
func (t *Tree) Signal(signal syscall.Signal) error {
	table, err := t.read()
	if err != nil {
		return err
	}
	groupSafe := false
	for _, p := range t.members {
		if same(p, table) && t.group > 0 && table[p.pid].group == t.group {
			groupSafe = true
			break
		}
	}
	if groupSafe {
		// Signal the verified group before its captured members can exit and be reaped.
		if err := t.kill(-t.group, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		// The group signal may have reaped parents before detached children are visited.
		table, err = t.read()
		if err != nil {
			return err
		}
	}
	for _, p := range t.members {
		if !same(p, table) || (groupSafe && table[p.pid].group == t.group) {
			continue
		}
		if err := t.kill(p.pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

func (t *Tree) alive() (bool, error) {
	table, err := t.read()
	if err != nil {
		return false, err
	}
	for _, p := range t.members {
		if same(p, table) {
			return true, nil
		}
	}
	return false, nil
}

// Wait escalates the captured tree after grace, then confirms it has disappeared.
func (t *Tree) Wait(ctx context.Context, grace time.Duration) error {
	for phase := 0; phase < 2; phase++ {
		deadline := time.Now().Add(grace)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			alive, err := t.alive()
			if err != nil {
				return err
			}
			if !alive {
				return nil
			}
			if !time.Now().Before(deadline) {
				break
			}
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if phase == 0 {
			if err := t.Signal(syscall.SIGKILL); err != nil {
				return err
			}
		}
	}
	return errors.New("captured processes still exist after SIGKILL")
}

func (t *Tree) Stop(ctx context.Context, grace time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	// Once parents receive TERM, finish cleanup even if the caller stops waiting.
	return t.Wait(context.WithoutCancel(ctx), grace)
}
