//go:build linux || darwin

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup starts cmd in a process group of its own and makes
// cancellation kill the whole group.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	setDeathSignal(cmd.SysProcAttr)
	cmd.Cancel = func() error {
		killGroup(cmd.Process.Pid)
		// Kill the leader directly too, in case it has already been reaped
		// from its group or the group kill was refused.
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		return nil
	}
}

// killGroup sends SIGKILL to the process group led by pid.
func killGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
