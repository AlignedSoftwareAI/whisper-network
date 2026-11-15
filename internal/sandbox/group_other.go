//go:build !linux && !darwin

package sandbox

import "os/exec"

// setProcessGroup does nothing: there are no Unix process groups here.
func setProcessGroup(*exec.Cmd) {}

// Wait waits for a command built by [Command].
func Wait(cmd *exec.Cmd) error { return cmd.Wait() }
