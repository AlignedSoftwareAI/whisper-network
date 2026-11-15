//go:build darwin

package sandbox

import (
	"os/exec"
	"syscall"
)

// setDeathSignal does nothing: darwin has no parent-death signal.
func setDeathSignal(*syscall.SysProcAttr) {}

// Wait waits for a command built by [Command]. Cancellation kills the
// child's process group; a child that exits on its own may leave
// descendants behind, which darwin offers no race-free way to find.
func Wait(cmd *exec.Cmd) error { return cmd.Wait() }
