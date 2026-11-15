package program

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
)

// skipOnWindows skips tests that exercise the worker or POSIX file modes,
// neither of which applies on Windows (the worker is unsupported there;
// see issue #26).
func skipOnWindows(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("not applicable on Windows: " + why)
	}
}

func TestWorkerUnsupportedOnWindows(t *testing.T) {
	assert.ErrorIs(t, workerSupported("windows"), errWorkerUnsupported)
	assert.ErrorContains(t, workerSupported("windows"), "issues/26")
	assert.NoError(t, workerSupported("linux"))
	assert.NoError(t, workerSupported("darwin"))
}
