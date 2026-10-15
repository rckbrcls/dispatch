package adapter

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

var ErrProcessesAlive = errors.New("agent process group still exists")

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
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			err = os.ErrProcessDone
		}
		escalation = make(chan error, 1)
		go func() {
			defer close(escalation)
			deadline := time.Now().Add(cmd.WaitDelay)
			for syscall.Kill(-cmd.Process.Pid, 0) != syscall.ESRCH {
				if time.Now().After(deadline) {
					if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
						escalation <- fmt.Errorf("%w: %v", ErrProcessesAlive, err)
						return
					}
					deadline = time.Now().Add(5 * time.Second)
					for syscall.Kill(-cmd.Process.Pid, 0) != syscall.ESRCH {
						if time.Now().After(deadline) {
							escalation <- ErrProcessesAlive
							return
						}
						time.Sleep(20 * time.Millisecond)
					}
					return
				}
				time.Sleep(20 * time.Millisecond)
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
