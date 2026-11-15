package program

import (
	"testing"

	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

// The test binary also serves as sandbox-exec and as the fake whisper-cli
// used by the bench tests; see workertest.Main.
func TestMain(m *testing.M) { workertest.Main(m) }
