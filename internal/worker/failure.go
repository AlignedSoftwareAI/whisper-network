package worker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/deweysasser/whisper-network/internal/client"
)

// childFailure turns a child's error into a task failure. Cancellation (lease
// gone, shutdown) passes through unchanged so handleLease can tell it apart.
// deterministic decides from the stderr tail whether the failure is a
// property of the input.
func childFailure(tool string, res childResult, err error, deterministic func(stderr string) bool) error {
	var ee *exec.ExitError
	switch {
	case errors.Is(err, errChildTimeout):
		return &taskError{reason: tool + " timed out", stderr: res.stderr, retryable: true, err: err}
	case errors.As(err, &ee):
		return &taskError{
			reason:    fmt.Sprintf("%s failed (%s)", tool, ee.ProcessState.String()),
			stderr:    res.stderr,
			retryable: deterministic == nil || !deterministic(res.stderr),
		}
	case errors.Is(err, context.Canceled), errors.Is(err, errLeaseGone), isFatal(err):
		return err
	}
	return &taskError{reason: "running " + tool, stderr: res.stderr, retryable: true, err: err}
}

// ffmpegInputErrors are stderr fragments that mean the media itself cannot be
// converted; every worker would fail the same way.
var ffmpegInputErrors = []string{
	"Invalid data found when processing input",
	"does not contain any stream",
	"matches no streams",
	"Could not find codec parameters",
	"could not find codec parameters",
}

// ffmpegInputError reports whether ffmpeg's stderr blames the input.
func ffmpegInputError(stderr string) bool {
	for _, s := range ffmpegInputErrors {
		if strings.Contains(stderr, s) {
			return true
		}
	}
	return false
}

// transferError classifies an error from downloading input or uploading a
// result: lease loss, rejection and cancellation pass through; anything else
// is a retryable failure.
func transferError(what string, err error) error {
	if client.IsLeaseGone(err) || client.IsResultInvalid(err) || isFatal(err) ||
		errors.Is(err, context.Canceled) || errors.Is(err, errLeaseGone) {
		return err
	}
	return retryableErr(what, err)
}
