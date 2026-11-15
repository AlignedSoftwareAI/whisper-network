package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// ExecArgs is the entry point of the sandbox-exec subcommand. args are the
// arguments after "sandbox-exec". On success it does not return on platforms
// with execve: the process image becomes the target command. Where there is
// no execve it runs the child and returns an [*ExitError] carrying a non-zero
// exit code.
func ExecArgs(args []string) error {
	inv, err := ParseArgs(args)
	if err != nil {
		return err
	}
	if inv.SelfTest {
		return runSelfTest(inv)
	}
	return Exec(inv.Spec, inv.Argv)
}

// Exec applies spec to the current process and then replaces it with argv.
// Failures to apply a security-relevant restriction (filesystem, network,
// rlimits, uid/gid) abort before the child starts. Failures to lower
// scheduling priority are only warnings, written to stderr.
func Exec(spec Spec, argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("sandbox-exec: %w", err)
	}
	// Nice value, scheduling policy and I/O priority are per-thread on
	// Linux and survive execve only on the thread that calls it, so the
	// goroutine must stay on this thread until the exec. Never unlocked.
	runtime.LockOSThread()
	warnings, err := apply(spec, path)
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "sandbox-exec: warning: %s\n", w)
	}
	if err != nil {
		return fmt.Errorf("sandbox-exec: %w", err)
	}
	return execve(path, argv)
}

// ExitError reports the exit status of a child run where execve is not
// available (Windows). The sandbox-exec command turns it into its own exit
// status.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("child exited with status %d", e.Code) }
