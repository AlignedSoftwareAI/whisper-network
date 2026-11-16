package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/rs/zerolog"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/client"
	"github.com/deweysasser/whisper-network/internal/sandbox"
)

// PrepareArgs is the ffmpeg command of DESIGN.md §7.2: the first audio
// stream, downmixed to 16 kHz mono s16 FLAC. "-loglevel error" keeps the
// input's metadata (titles, comments, encoder tags), which ffmpeg prints at
// the default level, out of the stderr tail that is logged and sent to the
// server.
func PrepareArgs(ffmpeg, in, out string) []string {
	return []string{ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", in,
		"-map", "0:a:0", "-vn", "-sn", "-dn", "-ac", "1", "-ar", "16000",
		"-sample_fmt", "s16", "-c:a", "flac", "-compression_level", "5", out}
}

// prepare converts the original media to canonical FLAC and uploads it.
func (w *Worker) prepare(ctx context.Context, l *api.Lease, dir string, run *leaseRun, log zerolog.Logger) (taskStats, error) {
	if w.tools.ffmpeg == "" {
		return taskStats{}, preWork("ffmpeg is not installed on this worker", nil)
	}
	if err := w.checkInputURL(l); err != nil {
		return taskStats{}, err
	}
	if l.InputBytes <= 0 {
		return taskStats{}, &taskError{reason: "lease carries no input_bytes; refusing an unbounded input",
			stage: api.StageInput}
	}
	in := filepath.Join(dir, "input")
	n, err := w.download(ctx, l, in, l.InputBytes, log)
	if err != nil {
		return taskStats{}, staged(api.StageInput, transferError("downloading input", err))
	}
	if err := checkInput(in, n, l); err != nil {
		return taskStats{}, staged(api.StageInput, err)
	}
	stats, err := w.prepareChild(ctx, l, dir, in, run, log)
	return stats, staged(api.StageChild, err)
}

// prepareChild runs ffmpeg on the downloaded input and uploads its output.
func (w *Worker) prepareChild(ctx context.Context, l *api.Lease, dir, in string, run *leaseRun, log zerolog.Logger) (taskStats, error) {

	out := filepath.Join(dir, "out.flac")
	lim := w.cfg.Limits
	spec := sandbox.Spec{
		ReadOnly:  slices.Concat(systemReadOnly, childSystemFiles, w.tools.ffmpegRO),
		ReadWrite: []string{dir},
		DenyNet:   true,
		FileSize:  uint64(lim.PrepareFileSize),
		OpenFiles: uint64(lim.OpenFiles),
		UID:       w.cfg.ChildUID,
		GID:       w.cfg.ChildGID,
	}.WithLowestPriority()
	w.limitProcesses(&spec)
	if !lim.NoAddressSpaceLimit {
		spec.AddressSpace = uint64(lim.PrepareAddressSpace)
	}
	res, err := w.runChild(ctx, childRun{argv: PrepareArgs(w.tools.ffmpeg, in, out), spec: spec, dir: dir, timeout: lim.PrepareTimeout})
	if err != nil {
		return taskStats{}, childFailure("ffmpeg", res, err, ffmpegInputError)
	}
	f, size, err := openOutput(out, lim.PrepareFileSize)
	switch {
	case errors.Is(err, errBadOutput):
		return taskStats{}, &taskError{reason: "ffmpeg output refused", stderr: res.stderr, err: err}
	case err != nil || size == 0:
		return taskStats{}, &taskError{reason: "ffmpeg produced no output", stderr: res.stderr, retryable: true, err: err}
	}
	defer f.Close()
	computeMS := res.elapsed.Milliseconds()
	run.uploading.Store(true)
	err = w.putWithRetry(ctx, log, func(ctx context.Context) error {
		return w.api.PutPrepareResult(ctx, l.ID, client.PrepareResult{
			Body: io.NewSectionReader(f, 0, size), Size: size, ComputeMS: computeMS, FFmpegVersion: w.tools.ffmpegVersion,
		})
	})
	if err != nil {
		return taskStats{}, transferError("uploading result", err)
	}
	return taskStats{computeMS: computeMS}, nil
}

// checkInput verifies a downloaded prepare input against the size and hash
// the lease advertised. A mismatch implicates the transfer, so it is
// retryable.
func checkInput(path string, n int64, l *api.Lease) error {
	if n != l.InputBytes {
		return &taskError{reason: fmt.Sprintf("input is %d bytes, lease says %d", n, l.InputBytes), retryable: true}
	}
	if l.InputSHA256 == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return retryableErr("reading input", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return retryableErr("hashing input", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != l.InputSHA256 {
		return &taskError{reason: "input sha256 " + got + " does not match the lease", retryable: true}
	}
	return nil
}
