// Package e2e holds the cross-component tests: the real server and real
// workers talking over HTTP in one process (DESIGN.md §12, "API" and
// "Worker" rows joined up). The harness is in e2etest; the real-engine
// Docker test is integration_test.go, behind the "integration" build tag.
package e2e

import (
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

// The test binary is also sandbox-exec and the fake whisper-cli and ffmpeg.
func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		// The worker is not supported on Windows (issue #26), and these
		// tests run real workers whose children need sandbox-exec.
		fmt.Println("skipping: the worker is not supported on Windows")
		os.Exit(0)
	}
	workertest.Main(m)
}
