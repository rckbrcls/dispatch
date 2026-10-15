package adapter

import (
	"os/exec"
	"syscall"
	"time"
)

// ownGroup runs cmd in its own process group and, on cancellation, kills
// the whole group so agent subprocesses do not outlive the run.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
}
