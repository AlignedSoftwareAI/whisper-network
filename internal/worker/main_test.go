package worker

import (
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

// The test binary doubles as sandbox-exec and as the fake whisper-cli and
// ffmpeg; see workertest.Main.
func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		// The worker is not supported on Windows (issue #26), and these
		// tests run real workers whose children need sandbox-exec.
		fmt.Println("skipping: the worker is not supported on Windows")
		os.Exit(0)
	}
	workertest.Main(m)
}
