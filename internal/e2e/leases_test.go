package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/client"
	"github.com/deweysasser/whisper-network/internal/e2e/e2etest"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
	"github.com/deweysasser/whisper-network/internal/worker"
	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

var hang = e2etest.ChildEnv(workertest.WhisperMode(workertest.ModeHang))

// leasedFor returns the task.leased events of a job, in order.
func leasedFor(t *testing.T, e *e2etest.Env, jobID string) []map[string]any {
	var out []map[string]any
	for _, ev := range e.Logs.Events(t, obs.EventTaskLeased) {
		if ev[obs.FieldJobID] == jobID {
			out = append(out, ev)
		}
	}
	return out
}

// TestCrashedWorkerLeaseExpires (scenario 4): a worker that dies mid-task
// without a word stops heartbeating; its lease expires after 3 intervals,
// counts as an attempt, and another worker completes the task.
func TestCrashedWorkerLeaseExpires(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t, e2etest.FastLeases)
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	doomed := e.StartWorker("doomed", hang)
	e.Approve(doomed.ID(), "public")
	first := e.WaitEvent(obs.EventTaskLeased, func(m map[string]any) bool { return m[obs.FieldJobID] == job.ID })
	doomed.WaitClaimed(first[obs.FieldLeaseID])
	doomed.Kill()

	heir := e.StartWorker("heir")
	e.Approve(heir.ID(), "public")
	exp := e.WaitEvent(obs.EventLeaseExpired, nil)
	assert.Equal(t, first[obs.FieldLeaseID], exp[obs.FieldLeaseID])
	assert.Equal(t, float64(1), exp[obs.FieldAttempt], "the expired lease counted")
	e.WaitJob(job.ID, api.JobDone)
	ls := leasedFor(t, e, job.ID)
	require.Len(t, ls, 2)
	assert.Equal(t, heir.ID(), ls[1][obs.FieldWorkerID])
	assert.Equal(t, float64(2), ls[1][obs.FieldAttempt])
	assert.Empty(t, e.Logs.Events(t, obs.EventLeaseReleased), "a crashed worker releases nothing")
	assert.Contains(t, e.Scrape(), `whisper_leases_ended_total{outcome="expired",kind="transcribe"} 1`)
}

// TestMaxAttemptsFailsJob (scenario 4): with --max-attempts 1 a single
// expired lease fails the job, and GET /job says why.
func TestMaxAttemptsFailsJob(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t, e2etest.FastLeases, func(_ *server.Config, s *store.Config) { s.MaxAttempts = 1 })
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	doomed := e.StartWorker("doomed", hang)
	e.Approve(doomed.ID(), "public")
	l := e.WaitEvent(obs.EventTaskLeased, func(m map[string]any) bool { return m[obs.FieldJobID] == job.ID })
	doomed.WaitClaimed(l[obs.FieldLeaseID])
	doomed.Kill()
	failed := e.WaitJob(job.ID, api.JobFailed)
	require.NotNil(t, failed.Error)
	assert.Equal(t, "transcribe task 1 failed after 1 attempt(s) (on worker "+doomed.ID()+"): lease expired; 0 of 1 tasks done, 0 cancelled", *failed.Error)
	assert.Equal(t, 1, failed.Tasks.Failed)
	assert.NotNil(t, failed.CompletedAt)
	e.WaitEvent(obs.EventJobFailed, func(m map[string]any) bool { return m[obs.FieldJobID] == job.ID })

	_, err := sub.Transcript(context.Background(), job.ID)
	assert.True(t, client.IsNotDone(err), "no transcript for a failed job: %v", err)
}

// TestGracefulShutdownReleases (scenario 5): on cancellation a worker kills
// its child and releases the lease, which requeues at once without
// consuming an attempt.
func TestGracefulShutdownReleases(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	w := e.StartWorker("leaving", hang)
	e.Approve(w.ID(), "public")
	l := e.WaitEvent(obs.EventTaskLeased, func(m map[string]any) bool { return m[obs.FieldJobID] == job.ID })
	w.WaitClaimed(l[obs.FieldLeaseID])
	start := time.Now()
	require.NoError(t, w.Stop())
	assert.Less(t, time.Since(start), 5*time.Second, "shutdown kills the hanging child promptly")
	rel := e.WaitEvent(obs.EventLeaseReleased, nil)
	assert.Equal(t, float64(0), rel[obs.FieldAttempt], "a release does not count")
	j := e.Job(job.ID)
	assert.Equal(t, 1, j.Tasks.Pending)

	w2 := e.StartWorker("next")
	e.Approve(w2.ID(), "public")
	e.WaitJob(job.ID, api.JobDone)
	ls := leasedFor(t, e, job.ID)
	require.Len(t, ls, 2)
	assert.Equal(t, float64(1), ls[1][obs.FieldAttempt], "the release refunded the attempt")
	assert.Empty(t, e.Logs.Events(t, obs.EventLeaseExpired))
}

// TestDrainFinishes (scenario 5): with --drain, cancellation lets the
// running task finish and upload before Run returns.
func TestDrainFinishes(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	w := e.StartWorker("draining", e2etest.ChildEnv(workertest.WhisperMode(workertest.SleepMode("1500ms"))),
		func(c *worker.Config) { c.Drain = true })
	e.Approve(w.ID(), "public")
	l := e.WaitEvent(obs.EventTaskLeased, func(m map[string]any) bool { return m[obs.FieldJobID] == job.ID })
	w.WaitClaimed(l[obs.FieldLeaseID])
	require.NoError(t, w.Stop())
	assert.Equal(t, api.JobDone, e.Job(job.ID).State, "the drained task finished before Run returned")
	assert.Empty(t, e.Logs.Events(t, obs.EventLeaseReleased))
	assert.Len(t, leasedFor(t, e, job.ID), 1)
}

// TestDeleteJobKillsChild (scenario 6): deleting a job mid-transcription
// makes the next heartbeat answer 410, and the worker kills its child
// promptly and goes back to claiming.
func TestDeleteJobKillsChild(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t, e2etest.FastLeases)
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	w := e.StartWorker("deleted-under", hang)
	e.Approve(w.ID(), "public")
	leased := e.WaitEvent(obs.EventTaskLeased, func(m map[string]any) bool { return m[obs.FieldJobID] == job.ID })
	// A heartbeat shows the input is downloaded and the child is running.
	e.Eventually(func() bool { return len(e.Requests("/lease/{id}/heartbeat", "worker:"+w.ID())) > 0 },
		"no heartbeat while the child runs")
	start := time.Now()
	require.NoError(t, sub.DeleteJob(context.Background(), job.ID))
	ev := w.WaitLog(obs.EventTaskFailed, func(m map[string]any) bool {
		return m[obs.FieldLeaseID] == leased[obs.FieldLeaseID] && m[obs.FieldOutcome] == string(api.OutcomeCancelled)
	})
	assert.Less(t, time.Since(start), 4*time.Second, "the child is killed within about one heartbeat")
	assert.NotNil(t, ev)
	assert.Contains(t, string(w.Logs.Bytes()), "server ended the lease; killing the task", "the heartbeat saw the 410")
	_, err := sub.Job(context.Background(), job.ID)
	assert.True(t, client.IsNotFound(err))

	// The worker is still healthy and takes the next job.
	next := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	e.WaitEvent(obs.EventTaskLeased, func(m map[string]any) bool { return m[obs.FieldJobID] == next.ID })
}

// TestChildCrashIsRetriedThenFails: a crashing whisper-cli is reported as a
// retryable failure with its stderr tail; the retry runs on the other
// worker, and once the task has failed on two workers the job fails with
// the reason.
func TestChildCrashIsRetriedThenFails(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	w := e.StartWorker("crashy", e2etest.ChildEnv(workertest.WhisperMode(workertest.ModeCrash)))
	e.Approve(w.ID(), "public")
	e.WaitEvent(obs.EventTaskFailed, func(m map[string]any) bool { return m[obs.FieldJobID] == job.ID })
	w2 := e.StartWorker("crashy too", e2etest.ChildEnv(workertest.WhisperMode(workertest.ModeCrash)))
	e.Approve(w2.ID(), "public")
	failed := e.WaitJob(job.ID, api.JobFailed)
	require.NotNil(t, failed.Error)
	assert.Contains(t, *failed.Error, "transcribe task 1 failed after 2 attempt(s) (on workers "+w.ID()+", "+w2.ID()+
		"): whisper-cli failed (exit status 3)")
	var evs []map[string]any
	e.Eventually(func() bool { // logged just after the job failed
		evs = e.Logs.Events(t, obs.EventTaskFailed)
		return len(evs) >= 2
	}, "one failure report per attempt")
	require.Len(t, evs, 2)
	for _, ev := range evs {
		assert.Equal(t, true, ev["retryable"])
		assert.Contains(t, ev[obs.FieldStderrTail], "simulated crash")
	}
	assert.Len(t, leasedFor(t, e, job.ID), 2)
}

// TestWorkerRestartKeepsIdentity: a worker restarted on the same state
// directory reuses its credentials (no new registration, no new approval)
// and PATCHes facts that changed; heartbeats carry its gauges.
func TestWorkerRestartKeepsIdentity(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t, e2etest.FastLeases)
	state := t.TempDir()
	first := e.StartWorker("restarting", func(c *worker.Config) { c.StateDir = state },
		e2etest.ChildEnv(workertest.WhisperMode(workertest.SleepMode("2500ms"))))
	id := first.ID()
	e.Approve(id, "public")
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	e.WaitJob(job.ID, api.JobDone)
	// The worker reported gauges. Which report is latest depends on timing:
	// a heartbeat during the task says one slot is busy, an idle claim
	// long-poll after it says none, so SlotsBusy is only checked for range.
	// The disk gauges are on every report.
	g := e.WorkerByDescription("restarting").Gauges
	require.NotNil(t, g, "the worker reported gauges")
	assert.GreaterOrEqual(t, g.SlotsBusy, 0)
	assert.LessOrEqual(t, g.SlotsBusy, 1, "one slot configured")
	assert.NotNil(t, g.ModelsBytes, "disk gauges reported")
	assert.NotNil(t, g.WorkBytes, "disk gauges reported")
	if assert.NotNil(t, g.StateFreeBytes, "disk gauges reported") {
		assert.Positive(t, *g.StateFreeBytes)
	}
	require.NoError(t, first.Stop())

	second := e.StartWorker("restarting", func(c *worker.Config) { c.StateDir = state; c.Slots = 2 })
	e.Eventually(func() bool { return e.WorkerByDescription("restarting").Facts.Slots == 2 },
		"the restarted worker never patched its facts")
	assert.Len(t, e.Workers(), 1, "no second registration")
	assert.Equal(t, id, second.ID())
	next := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	e.WaitJob(next.ID, api.JobDone)
	assert.Len(t, e.Logs.Events(t, obs.EventWorkerRegistered), 1)
}

// TestHeartbeatGoneDuringAcceptedUpload: the server accepts a result (the
// lease ends as done) while the worker is still waiting for the response,
// and meanwhile a heartbeat answers 410. The upload's own answer is
// authoritative: the worker must record the task as done, not abandon it.
// (Found under load in TestServerRestartWithLiveLease.)
func TestHeartbeatGoneDuringAcceptedUpload(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
	w := e.StartWorker("slow-ack")
	release := w.Net.HoldResponses("/result")
	defer release()
	id := w.ID()
	e.Approve(id, "public")
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	e.WaitJob(job.ID, api.JobDone)
	e.Eventually(func() bool {
		for _, r := range e.Requests("/lease/{id}/heartbeat", "worker:"+id) {
			if r[obs.FieldStatus] == float64(410) {
				return true
			}
		}
		return false
	}, "no heartbeat after the result was accepted")
	release()
	w.WaitLog(obs.EventTaskDone, nil)
	assert.Empty(t, w.Logs.Events(t, obs.EventTaskFailed), "the worker abandoned a task the server had accepted")
}
