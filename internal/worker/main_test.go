package worker

import (
	"testing"

	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

// The test binary doubles as sandbox-exec and as the fake whisper-cli and
// ffmpeg; see workertest.Main.
func TestMain(m *testing.M) { workertest.Main(m) }
