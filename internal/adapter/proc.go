package adapter

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"dispatch/internal/process"
)

var ErrProcessesAlive = errors.New("agent process cleanup incomplete")

// ownGroup retains immediate group cleanup for short quota probes.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
}

// runAgent also escalates descendants: WaitDelay alone only kills the leader.
func runAgent(cmd *exec.Cmd, started func(int) error) error {
	ownGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	var escalation chan error
	cmd.Cancel = func() error {
		tree, err := process.Capture(cmd.Process.Pid)
		escalation = make(chan error, 1)
		if err != nil {
			// Do not claim successful cancellation when ancestry cannot be inspected.
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			escalation <- fmt.Errorf("%w: %v", ErrProcessesAlive, err)
			close(escalation)
			return err
		}
		err = tree.Signal(syscall.SIGTERM)
		go func() {
			defer close(escalation)
			waitErr := tree.Wait(context.Background(), cmd.WaitDelay)
			if cleanupErr := errors.Join(err, waitErr); cleanupErr != nil {
				escalation <- fmt.Errorf("%w: %v", ErrProcessesAlive, cleanupErr)
			}
		}()
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if started != nil {
		if err := started(cmd.Process.Pid); err != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			cmd.Wait()
			return err
		}
	}
	err := cmd.Wait()
	// Wait synchronizes with CommandContext's cancellation watcher.
	if escalation != nil {
		if groupErr := <-escalation; groupErr != nil {
			return groupErr
		}
	}
	return err
}
