package e2e

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/e2e/e2etest"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/sandbox"
	"github.com/deweysasser/whisper-network/internal/worker"
	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

// sandboxLevel makes a worker report level instead of probing.
func sandboxLevel(level api.SandboxLevel) func(*worker.Config) {
	return func(c *worker.Config) {
		c.ProbeSandbox = func(context.Context) (sandbox.ProbeResult, error) {
			return sandbox.ProbeResult{Level: level}, nil
		}
	}
}

// TestPreparePath (scenario 2): uploads that are not canonical — a 44.1 kHz
// stereo WAV and an MP4-looking blob — are converted by a worker's
// (fake) ffmpeg under sandbox-exec, the FLAC is accepted, split and
// transcribed. A worker whose sandbox level is below --prepare-min-sandbox
// is never given the prepare task, though it may transcribe.
func TestPreparePath(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
	sub := e.Submitter("public")
	stereo := e.Submit(sub, "public", api.JobParams{Label: "stereo"}, e2etest.StereoWAV(3))
	mp4 := e.Submit(sub, "public", api.JobParams{Label: "mp4"}, e2etest.MP4Like(96000))
	for _, j := range []*api.Job{stereo, mp4} {
		assert.Equal(t, api.JobPreparing, j.State)
		assert.Equal(t, 1, j.Tasks.Total, "just the prepare task")
	}
	assert.Equal(t, "audio/wav", stereo.Media.OriginalType)
	assert.Equal(t, "video/mp4", mp4.Media.OriginalType)

	// A worker that can only reach fs-only is offered nothing here: its
	// claims run (and time out) while both jobs wait in preparing.
	weak := e.StartWorker("weak", sandboxLevel(api.SandboxFSOnly))
	weakID := weak.ID()
	assert.Equal(t, api.SandboxFSOnly, e.WorkerByDescription("weak").Facts.SandboxLevel)
	e.Approve(weakID, "public")
	e.Eventually(func() bool {
		n := 0
		for _, r := range e.Requests("/worker/{id}/leases", "worker:"+weakID) {
			if r[obs.FieldStatus] == float64(204) {
				n++
			}
		}
		return n >= 2
	}, "the weak worker never polled twice")
	assert.Equal(t, api.JobPreparing, e.Job(stereo.ID).State)
	assert.Equal(t, api.JobPreparing, e.Job(mp4.ID).State)
	assert.Empty(t, e.Logs.Events(t, obs.EventTaskLeased), "no prepare lease for a worker below the policy")

	// A full-sandbox worker (the real probe on this machine) prepares both.
	strong := e.StartWorker("strong")
	strongID := strong.ID()
	requirePrepareSandbox(t, e, "strong")
	e.Approve(strongID, "public")
	for _, j := range []*api.Job{stereo, mp4} {
		done := e.WaitJob(j.ID, api.JobDone)
		assert.True(t, done.Media.OriginalPurged, "the original is purged once canonical audio exists")
		assert.Positive(t, done.Media.DurationS)
		txt := e.Transcript(sub, j.ID)
		assert.Contains(t, strings.SplitN(txt, "\n", 2)[0], "; ffmpeg="+workertest.FakeFFmpegVersion+"; ",
			"the provenance names the ffmpeg that prepared it")
	}
	for _, ev := range e.Logs.Events(t, obs.EventTaskLeased) {
		if ev[obs.FieldTaskKind] == string(api.KindPrepare) {
			assert.Equal(t, strongID, ev[obs.FieldWorkerID], "prepare went only to the full-sandbox worker")
		}
	}
	require.NotEmpty(t, e.Logs.Events(t, obs.EventJobPrepared))
}

// requirePrepareSandbox skips a test that needs a prepare task when this
// platform's worker sandbox is below the default prepare policy (macOS and
// Windows have no Landlock). Such a worker is correctly never given prepare
// tasks, so the test would only wait for work that never comes.
func requirePrepareSandbox(t *testing.T, e *e2etest.Env, description string) {
	t.Helper()
	if lvl := e.WorkerByDescription(description).Facts.SandboxLevel; lvl != api.SandboxFull {
		t.Skipf("this platform's sandbox level is %s, below the default prepare policy", lvl)
	}
}

// TestPrepareFailureSurfaces: ffmpeg blaming the input is not retried; the
// job fails with ffmpeg's reason in GET /job.
func TestPrepareFailureSurfaces(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
	w := e.StartWorker("w", e2etest.ChildEnv(workertest.FFmpegMode(workertest.ModeInvalid)))
	requirePrepareSandbox(t, e, "w")
	e.Approve(w.ID(), "public")
	sub := e.Submitter("public")
	j := e.Submit(sub, "public", api.JobParams{}, e2etest.MP4Like(4096))
	failed := e.WaitJob(j.ID, api.JobFailed)
	require.NotNil(t, failed.Error)
	assert.Contains(t, *failed.Error, "ffmpeg failed")
	assert.Equal(t, 1, failed.Tasks.Failed)
	ev := e.WaitEvent(obs.EventTaskFailed, nil)
	assert.Contains(t, ev[obs.FieldStderrTail], "Invalid data found when processing input")
	assert.Equal(t, false, ev["retryable"])
}

// TestInterruptedInputDownloadsResume: a prepare input cut mid-stream
// resumes with a Range request (206); a transcribe input, which the server
// regenerates rather than ranges, restarts from byte 0 (200). Neither
// costs an attempt.
func TestInterruptedInputDownloadsResume(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
	w := e.StartWorker("flaky-net")
	requirePrepareSandbox(t, e, "flaky-net")
	w.Net.CutBodyOnce("/input", 50000)
	e.Approve(w.ID(), "public")
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.StereoWAV(2))
	e.WaitEvent(obs.EventJobPrepared, func(m map[string]any) bool { return m[obs.FieldJobID] == job.ID })
	w.Net.CutBodyOnce("/input", 20000)
	done := e.WaitJob(job.ID, api.JobDone)
	transcribes := done.Tasks.Total - 1

	var codes []float64
	e.Eventually(func() bool { // each request is logged as its handler returns
		codes = codes[:0]
		for _, r := range e.Requests("/lease/{id}/input", "") {
			codes = append(codes, r[obs.FieldStatus].(float64))
		}
		return len(codes) >= 3+transcribes
	}, "input requests")
	// Prepare: the cut 200 and a ranged 206; the first transcribe input:
	// a cut 200 and a full 200 restart; every other transcribe input: 200.
	sort.Float64s(codes)
	want := []float64{200, 200} // the two cut responses
	for range transcribes {
		want = append(want, 200)
	}
	assert.Equal(t, append(want, 206), codes)
	assert.Empty(t, e.Logs.Events(t, obs.EventTaskFailed))
	assert.Len(t, leasedFor(t, e, job.ID), done.Tasks.Total, "one lease per task")
}
