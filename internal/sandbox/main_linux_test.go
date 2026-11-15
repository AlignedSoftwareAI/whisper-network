//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"testing"
)

// helperEnv marks a re-executed test binary that should act as the sandboxed
// target instead of running tests.
const helperEnv = "WN_SANDBOX_TEST_HELPER"

// TestMain lets the test binary stand in for the real program: it answers
// "sandbox-exec" like the production subcommand and, when helperEnv is set,
// behaves as a small target program (see helper_test.go).
func TestMain(m *testing.M) {
	switch {
	case len(os.Args) > 1 && os.Args[1] == SubcommandName:
		if err := ExecArgs(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(126)
		}
		os.Exit(0)
	case os.Getenv(helperEnv) != "":
		os.Exit(runHelper(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fastExit stops a race-instrumented helper from sleeping a second at exit
// (GORACE atexit_sleep_ms defaults to 1000).
const fastExit = "GORACE=atexit_sleep_ms=0"
