//go:build linux

package sandbox

import (
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// setDeathSignal kills the child when the worker thread that started it
// dies. sandbox-exec renews it after changing uid, which clears it.
func setDeathSignal(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }

// Wait waits for a command built by [Command] and then kills whatever is
// left of its process group: descendants that outlived the child would
// otherwise keep running unsupervised.
//
// It first waits for the child to exit without reaping it (waitid with
// WNOWAIT). While the zombie exists its pid cannot be reused, so the group
// kill cannot hit an unrelated process.
func Wait(cmd *exec.Cmd) error {
	if cmd.Process != nil {
		pid := cmd.Process.Pid
		var info unix.Siginfo
		for {
			err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if !errors.Is(err, unix.EINTR) {
				break
			}
		}
		killGroup(pid)
	}
	return cmd.Wait()
}
