package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dispatch/internal/store"
	"dispatch/internal/worktree"
)

func processStart(ctx context.Context, pid int) string {
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func alive(pid int) bool {
	if pid == 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func signalProcess(pid int, signal syscall.Signal) error {
	if pid == 0 {
		return nil
	}
	err := syscall.Kill(pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Reconcile preserves orphaned work so it can be inspected or recovered.
func (d Deps) Reconcile(ctx context.Context) error {
	recs, err := d.Store.List()
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.Status != store.Pending && rec.Status != store.Running {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		note := "runner process exited unexpectedly"
		if rec.PID > 0 && alive(rec.PID) {
			if rec.ProcStart == "" {
				continue
			} // Legacy records have no identity.
			start := processStart(ctx, rec.PID)
			if err := ctx.Err(); err != nil {
				return err
			}
			if start == "" && alive(rec.PID) {
				return fmt.Errorf("cannot verify process identity for run %s", rec.ID)
			}
			if start == rec.ProcStart {
				continue
			}
			note = "runner process identity changed (PID reused)"
		}
		// Avoid replacing a final record written while ps was running.
		latest, err := d.Store.Get(rec.ID)
		if err != nil {
			return err
		}
		if latest.Status != rec.Status || latest.PID != rec.PID || latest.ProcStart != rec.ProcStart {
			continue
		}
		rec = latest
		if rec.AgentPID > 0 && alive(-rec.AgentPID) {
			if err := signalProcess(-rec.AgentPID, syscall.SIGTERM); err != nil {
				return err
			}
		}
		rec.Status, rec.Note, rec.FinishedAt = store.Orphaned, joinNote(rec.Note, note), d.Now()
		rec.ExitCode = 1
		if err := d.Store.Append(rec); err != nil {
			return err
		}
	}
	return nil
}

// Cancel waits for both the owner and the agent group before recording success.
func (d Deps) Cancel(ctx context.Context, id string) (store.Record, error) {
	if err := d.Reconcile(ctx); err != nil {
		return store.Record{}, err
	}
	rec, err := d.Store.Get(id)
	if err != nil {
		return rec, err
	}
	orphan := rec.Status == store.Orphaned
	if terminal(rec.Status) && !orphan {
		return rec, fmt.Errorf("run %s already %s", rec.ID, rec.Status)
	}
	ownerPID := rec.PID
	if orphan {
		ownerPID = 0
	}
	if rec.AgentPID > 0 {
		if err := signalProcess(-rec.AgentPID, syscall.SIGTERM); err != nil {
			return rec, err
		}
	}
	if ownerPID > 0 {
		if err := signalProcess(ownerPID, syscall.SIGTERM); err != nil {
			return rec, err
		}
	}
	stopped := func() bool { return !alive(ownerPID) && (rec.AgentPID <= 0 || !alive(-rec.AgentPID)) }
	for phase := 0; phase < 2; phase++ {
		deadline := time.NewTimer(5 * time.Second)
		tick := time.NewTicker(25 * time.Millisecond)
		waiting := true
		for waiting && !stopped() {
			select {
			case <-ctx.Done():
				deadline.Stop()
				tick.Stop()
				return rec, ctx.Err()
			case <-deadline.C:
				waiting = false
			case <-tick.C:
			}
		}
		deadline.Stop()
		tick.Stop()
		if stopped() {
			if latest, err := d.Store.Get(id); err != nil {
				return rec, err
			} else {
				rec = latest
			}
			if orphan {
				return rec, nil
			}
			rec.Status, rec.Error, rec.FinishedAt = store.Cancelled, "cancelled", d.Now()
			return rec, d.Store.Append(rec)
		}
		if phase == 0 {
			if rec.AgentPID > 0 {
				if err := signalProcess(-rec.AgentPID, syscall.SIGKILL); err != nil {
					return rec, err
				}
			}
			// Recheck identity before escalating the owner.
			if ownerPID > 0 && (rec.ProcStart == "" || processStart(ctx, ownerPID) == rec.ProcStart) {
				if err := signalProcess(ownerPID, syscall.SIGKILL); err != nil {
					return rec, err
				}
			}
		}
	}
	return rec, fmt.Errorf("run %s processes still exist after cancellation", rec.ID)
}

// Recover commits preserved work, without claiming that it was verified.
func (d Deps) Recover(ctx context.Context, id string) (store.Record, string, error) {
	rec, err := d.Store.Get(id)
	if err != nil {
		return rec, "", err
	}
	if rec.Status != store.Orphaned {
		return rec, "", fmt.Errorf("run %s is %s; recovery requires ORPHANED", rec.ID, rec.Status)
	}
	if rec.Worktree == "" {
		return rec, "", fmt.Errorf("run %s has no worktree", rec.ID)
	}
	if info, err := os.Stat(rec.Worktree); err != nil || !info.IsDir() {
		return rec, "", fmt.Errorf("worktree %s does not exist", rec.Worktree)
	}
	if err := ctx.Err(); err != nil {
		return rec, "", err
	}
	if rec.AgentPID > 0 && alive(-rec.AgentPID) {
		return rec, "", fmt.Errorf("agent group still exists; cancel run %s before recovery", rec.ID)
	}
	stat, err := worktree.Recover(rec.Worktree, rec.Branch, rec.BaseSHA, rec.ID)
	if err != nil {
		return rec, "", err
	}
	rec.Status, rec.Note, rec.FinishedAt = store.Completed, "recovered from orphan; unverified", d.Now()
	rec.Error, rec.ExitCode = "", 0
	return rec, stat, d.Store.Append(rec)
}
