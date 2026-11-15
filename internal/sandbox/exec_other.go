//go:build !linux && !darwin

package sandbox

import (
	"errors"
	"os"
	"os/exec"
)

// execve has no equivalent here, so it runs the child with inherited standard
// streams and forwards its exit status.
func execve(path string, argv []string) error {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &ExitError{Code: ee.ExitCode()}
	}
	if err != nil {
		return err
	}
	return &ExitError{Code: 0}
}
