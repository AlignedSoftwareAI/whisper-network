package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/client"
	"github.com/deweysasser/whisper-network/internal/obs/obstest"
	"github.com/deweysasser/whisper-network/internal/sandbox"
	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

func TestRegistrationPersistsAndIsReused(t *testing.T) {
	h := newHarness(t)
	h.srv.FailRegistrations(2)
	cancel, done := h.run()
	id := h.activate()
	h.eventually(func() bool { return len(h.srv.Claims()) > 0 }, "never claimed")
	cancel()
	require.NoError(t, waitDone(t, done))

	path := filepath.Join(h.cfg.StateDir, CredentialsFile)
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	creds, err := LoadCredentials(path)
	require.NoError(t, err)
	assert.Equal(t, id, creds.ID)
	assert.Equal(t, h.srv.URL, creds.Server)
	w, _ := h.srv.OnlyWorker()
	assert.Equal(t, 0, w.Patches, "freshly registered facts need no patch")
	assert.Equal(t, "test", w.Facts.Version)

	// Restart with changed facts: same identity, one PATCH.
	h.cfg.Description = "garage NUC, nights only"
	cancel, done = h.run()
	h.eventually(func() bool { w, _ := h.srv.OnlyWorker(); return w.Patches == 1 }, "facts not patched")
	cancel()
	require.NoError(t, waitDone(t, done))
	ws := h.srv.Workers()
	require.Len(t, ws, 1, "a restart must not register again")
	assert.Equal(t, "garage NUC, nights only", ws[0].Facts.Description)

	logs := h.logs.Bytes()
	obstest.AssertClean(t, logs)
	assert.NotContains(t, string(logs), creds.Token)
}

func TestCredentialsForAnotherServerAreRefused(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, SaveCredentials(filepath.Join(h.cfg.StateDir, CredentialsFile),
		&Credentials{Server: "https://other.example", ID: "w9", Token: "wnw_elsewhere"}))
	_, done := h.run()
	err := waitDone(t, done)
	assert.ErrorContains(t, err, "belongs to server https://other.example")
	assert.Empty(t, h.srv.Workers())
}

func TestPendingBackoffThenActivation(t *testing.T) {
	h := newHarness(t)
	_, _ = h.run()
	w := h.worker()
	// Let the worker poll a few times while pending.
	h.eventually(func() bool { return len(h.sleeps.all()) >= 4 }, "no pending polls")
	h.srv.SetState(w.ID, api.WorkerActive)
	h.eventually(func() bool { return len(h.srv.Claims()) > 0 }, "never claimed after approval")

	sl := h.sleeps.all()
	assert.Equal(t, []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}, sl[:4])
	ev := 0
	for _, line := range splitLines(h.logs.Bytes()) {
		if line["_msg"] == "worker awaiting approval; approve this worker_id on the server" ||
			line["message"] == "worker awaiting approval; approve this worker_id on the server" {
			ev++
			assert.Equal(t, w.ID, line["worker_id"])
		}
	}
	assert.Equal(t, 1, ev, "the approval hint is logged exactly once")
}

func TestPausedWorkerPollsSlowlyUntilResumed(t *testing.T) {
	h := newHarness(t)
	h.srv.PollIntervalS = 45
	_, _ = h.run()
	w := h.worker()
	h.srv.SetState(w.ID, api.WorkerPaused)
	h.eventually(func() bool { return len(h.srv.Claims()) >= 3 }, "paused worker stopped polling")
	assert.Contains(t, h.sleeps.all(), 45*time.Second, "uses the server's poll interval hint")
	task := h.srv.AddTask(workertest.Task{Kind: api.KindTranscribe, Input: workertest.WAV(1), Model: "tiny.test"})
	h.srv.SetState(w.ID, api.WorkerActive)
	h.eventually(h.taskState(task, api.TaskDone), "resumed worker did not work")
}

func TestRejectedWhileRunningKillsChildAndExits(t *testing.T) {
	h := newHarness(t)
	h.cfg.ChildEnv = []string{workertest.WhisperMode(workertest.ModeHang)}
	task := h.srv.AddTask(workertest.Task{Kind: api.KindTranscribe, Input: workertest.WAV(1), Model: "tiny.test"})
	_, done := h.run()
	id := h.activate()
	h.liveLease(task)
	h.srv.SetState(id, api.WorkerRejected)
	err := waitDone(t, done)
	assert.ErrorIs(t, err, ErrRejected)
	assert.Equal(t, api.OutcomeCancelled, h.srv.LeasesFor(task)[0].Outcome, "no release attempted")
}

func TestRejectedWhilePendingExits(t *testing.T) {
	h := newHarness(t)
	_, done := h.run()
	h.srv.SetState(h.worker().ID, api.WorkerRejected)
	assert.ErrorIs(t, waitDone(t, done), ErrRejected)
}

func TestRevokedTokenExitsWithoutReregistering(t *testing.T) {
	h := newHarness(t)
	_, done := h.run()
	id := h.activate()
	h.eventually(func() bool { return len(h.srv.Claims()) > 0 }, "never claimed")
	h.srv.Revoke(id)
	err := waitDone(t, done)
	assert.ErrorIs(t, err, ErrUnknownWorker)
	assert.ErrorContains(t, err, CredentialsFile)
	assert.Len(t, h.srv.Workers(), 1)
}

func TestFactsProbe(t *testing.T) {
	h := newHarness(t)
	h.cfg.Slots = 0
	h.cfg.Memory = 0 // from fakeRoot: 16384000 kB
	h.cfg.ThreadsPerSlot = 2
	_, _ = h.run()
	w := h.worker()
	f := w.Facts
	assert.Equal(t, "Fake CPU @ 3GHz", f.CPUModel)
	assert.Equal(t, 8, f.Cores)
	assert.Equal(t, int64(16384000*1024), f.MemBytes)
	assert.Equal(t, "whisper.cpp "+workertest.FakeWhisperVersion, f.EngineVersion)
	assert.Equal(t, workertest.FakeFFmpegVersion, f.FFmpegVersion)
	assert.Equal(t, api.SandboxFull, f.SandboxLevel)
	assert.Equal(t, []api.TaskKind{api.KindPrepare, api.KindTranscribe}, f.Kinds)
	assert.Equal(t, []string{"tiny.test", "big.test"}, f.Models)
	// budget ≈ 14.6 GiB / 3 GiB (big.test) = 4; cpu 8/2 = 4.
	assert.Equal(t, 4, f.Slots)
	assert.Equal(t, f.Hostname, f.Description)
}

func TestFactsProbeFallbacks(t *testing.T) {
	h := newHarness(t)
	h.cfg.FFmpeg = filepath.Join(t.TempDir(), "no-ffmpeg")
	h.cfg.ChildEnv = []string{workertest.WhisperMode(workertest.ModeNoVersion)}
	_, _ = h.run()
	f := h.worker().Facts
	assert.Equal(t, UnknownEngine, f.EngineVersion)
	assert.Equal(t, []api.TaskKind{api.KindTranscribe}, f.Kinds, "no ffmpeg, no prepare")
	assert.Empty(t, f.FFmpegVersion)

	h2 := newHarness(t)
	h2.cfg.EngineVersion = "whisper.cpp 1.9.5-custom"
	h2.cfg.Kinds = []api.TaskKind{api.KindPrepare}
	_, _ = h2.run()
	f = h2.worker().Facts
	assert.Equal(t, "whisper.cpp 1.9.5-custom", f.EngineVersion)
	assert.Equal(t, []api.TaskKind{api.KindPrepare}, f.Kinds)
	assert.Empty(t, f.Models)
}

func TestNothingToOfferFails(t *testing.T) {
	h := newHarness(t)
	h.cfg.Memory = 1*gib + 50*mib // budget 50 MiB: no model, no prepare
	_, done := h.run()
	assert.ErrorContains(t, waitDone(t, done), "nothing to offer")

	// Without /proc/meminfo the worker cannot size itself and must ask for
	// --memory. Platforms that report memory another way (sysctl on macOS)
	// start normally instead, so this half applies only where they don't.
	if platformMemory() != 0 {
		return
	}
	h = newHarness(t)
	h.cfg.Memory = 0
	h.cfg.SysRoot = fakeRootWithout("proc/meminfo")
	_, done = h.run()
	assert.ErrorContains(t, waitDone(t, done), "--memory")
}

func TestSandboxProbeFailureReportsNone(t *testing.T) {
	h := newHarness(t)
	h.cfg.ProbeSandbox = func(context.Context) (sandbox.ProbeResult, error) {
		return sandbox.ProbeResult{}, assert.AnError
	}
	_, _ = h.run()
	assert.Equal(t, api.SandboxNone, h.worker().Facts.SandboxLevel)
	ev := findEvent(t, h.logs.Bytes(), "worker.sandbox")
	assert.Equal(t, "warn", ev["level"])
}

// The real self-test re-executes the test binary as sandbox-exec.
func TestRealSandboxProbe(t *testing.T) {
	h := newHarness(t)
	h.cfg.ProbeSandbox = nil
	_, _ = h.run()
	f := h.worker().Facts
	assert.NotEqual(t, api.SandboxLevel(""), f.SandboxLevel)
	ev := findEvent(t, h.logs.Bytes(), "worker.sandbox")
	assert.Equal(t, string(f.SandboxLevel), ev["sandbox_level"])
}

func TestNewRequiresClientAndState(t *testing.T) {
	_, err := New(Config{StateDir: t.TempDir()})
	assert.Error(t, err)
	c, _ := client.New("http://x")
	_, err = New(Config{Client: c})
	assert.Error(t, err)
}

func TestDefaults(t *testing.T) {
	c, _ := client.New("http://x")
	cfg := Config{Client: c, StateDir: "/s"}
	require.NoError(t, cfg.setDefaults())
	assert.Equal(t, 4, cfg.ThreadsPerSlot)
	assert.Equal(t, gib, cfg.MemoryReserve)
	assert.Equal(t, 30*time.Second, cfg.PendingBackoff.Delay(0).Round(10*time.Second))
	assert.Equal(t, 5*time.Minute, cfg.PendingBackoff.Max)
	assert.Equal(t, 30*time.Second, cfg.ClaimWait)
	assert.Equal(t, []api.TaskKind{api.KindPrepare, api.KindTranscribe}, cfg.Kinds)
	assert.Equal(t, 4.0, cfg.Limits.TimeoutFactor)
	assert.Equal(t, 10*time.Minute, cfg.Limits.TimeoutBase)
}

func fakeRootWithout(name string) fstest.MapFS {
	r := fakeRoot()
	delete(r, name)
	return r
}

func splitLines(b []byte) []map[string]any {
	var out []map[string]any
	for _, line := range bytes.Split(b, []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}
