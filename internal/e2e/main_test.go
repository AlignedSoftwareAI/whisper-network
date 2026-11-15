// Package e2e holds the cross-component tests: the real server and real
// workers talking over HTTP in one process (DESIGN.md §12, "API" and
// "Worker" rows joined up). The harness is in e2etest; the real-engine
// Docker test is integration_test.go, behind the "integration" build tag.
package e2e

import (
	"testing"

	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

// The test binary is also sandbox-exec and the fake whisper-cli and ffmpeg.
func TestMain(m *testing.M) { workertest.Main(m) }
