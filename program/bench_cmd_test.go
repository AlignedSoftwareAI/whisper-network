package program

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/models"
	"github.com/deweysasser/whisper-network/internal/worker"
	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

var benchModelData = bytes.Repeat([]byte("bench-model "), 64)

func benchCmd(t *testing.T, args ...string) (*BenchCmd, *bytes.Buffer) {
	t.Helper()
	var opts Options
	_, err := opts.Parse(append([]string{"bench"}, args...))
	require.NoError(t, err)
	c := &opts.Bench
	var out bytes.Buffer
	c.out = &out
	c.Cache = t.TempDir()
	c.catalog = worker.Catalog{pinnedModel("bench.test", benchModelData)}
	c.fetch = func(context.Context, models.Model) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(benchModelData)), nil
	}
	return c, &out
}

func pinnedModel(name string, data []byte) models.Model {
	return models.Model{Name: name, Kind: models.KindWhisper, File: "ggml-" + name + ".bin",
		SHA256: sha256Hex(data), Size: int64(len(data)), PeakRSS: 1 << 20}
}

func TestBenchWithSyntheticAudio(t *testing.T) {
	skipOnWindows(t, "bench runs whisper-cli through the worker's tooling")
	fakes := workertest.InstallFakes(t)
	c, out := benchCmd(t, "--model", "bench.test", "--threads", "1,2", "--whisper-cli", fakes.WhisperCLI)
	require.NoError(t, c.Run(context.Background()))
	s := out.String()
	assert.Contains(t, s, "synthetic")
	assert.Contains(t, s, "not accuracy")
	assert.Regexp(t, `(?m)^\s+1\s+\S+\s+[0-9.]+\s+[0-9.]+\s+\d+ MiB`, s, "peak RSS from rusage")
	assert.Regexp(t, `suggested: --threads-per-slot [12]\n`, s)
	_, err := os.Stat(filepath.Join(c.Cache, "ggml-bench.test.bin"))
	assert.NoError(t, err, "model cached after verification")
}

func TestBenchRejectsTamperedModel(t *testing.T) {
	skipOnWindows(t, "bench runs whisper-cli through the worker's tooling")
	fakes := workertest.InstallFakes(t)
	c, _ := benchCmd(t, "--model", "bench.test", "--whisper-cli", fakes.WhisperCLI)
	c.fetch = func(context.Context, models.Model) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("evil")), nil
	}
	assert.ErrorIs(t, c.Run(context.Background()), worker.ErrModelMismatch)
}

func TestBenchNeedsWhisperCLI(t *testing.T) {
	c, _ := benchCmd(t, "--model", "bench.test", "--whisper-cli", filepath.Join(t.TempDir(), "absent"))
	assert.ErrorContains(t, c.Run(context.Background()), "whisper-cli not found")
	c, _ = benchCmd(t, "--model", "nope", "--whisper-cli", os.Args[0])
	assert.ErrorContains(t, c.Run(context.Background()), "unknown model")
}

func TestBenchAudioFile(t *testing.T) {
	skipOnWindows(t, "bench runs whisper-cli through the worker's tooling")
	fakes := workertest.InstallFakes(t)
	wav := filepath.Join(t.TempDir(), "clip.wav")
	require.NoError(t, os.WriteFile(wav, workertest.WAV(2), 0o600))
	c, out := benchCmd(t, "--model", "bench.test", "--threads", "4", "--audio", wav, "--whisper-cli", fakes.WhisperCLI)
	require.NoError(t, c.Run(context.Background()))
	assert.NotContains(t, out.String(), "synthetic")

	bad := filepath.Join(t.TempDir(), "x.mp3")
	require.NoError(t, os.WriteFile(bad, []byte("ID3...."), 0o600))
	c, _ = benchCmd(t, "--model", "bench.test", "--audio", bad, "--whisper-cli", fakes.WhisperCLI)
	assert.ErrorContains(t, c.Run(context.Background()), "16 kHz mono")
}

func TestSyntheticWAVIsCanonical(t *testing.T) {
	w, err := media.OpenWAV(bytes.NewReader(syntheticWAV(3)), 0)
	require.NoError(t, err)
	assert.Equal(t, 3*time.Second, w.Duration())
}

func TestReportSuggestion(t *testing.T) {
	// 8 cores, 60 s audio. 2 threads: 4 slots × 60/30 = 8× ; 4 threads:
	// 2 × 60/16 = 7.5× (within 5 %? no: 6.25 % below) ; 8 threads: 1 × 60/10 = 6×.
	runs := []benchRun{{2, 30 * time.Second, 0}, {4, 16 * time.Second, 0}, {8, 10 * time.Second, 0}}
	var out bytes.Buffer
	require.NoError(t, report(&out, runs, 60, 8))
	assert.Contains(t, out.String(), "suggested: --threads-per-slot 2")

	// Within 5 %, prefer more threads for lower latency.
	runs[1].wall = 15400 * time.Millisecond // 2 × 60/15.4 = 7.79×, 2.6 % below 8×
	out.Reset()
	require.NoError(t, report(&out, runs, 60, 8))
	assert.Contains(t, out.String(), "suggested: --threads-per-slot 4")
	assert.Contains(t, out.String(), "n/a")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
