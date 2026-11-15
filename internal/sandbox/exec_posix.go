//go:build linux || darwin

package sandbox

import (
	"os"
	"syscall"
)

// execve replaces the process image with the target.
func execve(path string, argv []string) error {
	return syscall.Exec(path, argv, os.Environ())
}
