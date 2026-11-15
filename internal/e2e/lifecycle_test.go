package e2e

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/e2e/e2etest"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/worker"
	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

// TestWorkerApprovalLifecycle (scenario 3) drives one worker through
// pending → active → paused → active → rejected with the admin API.
func TestWorkerApprovalLifecycle(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
	sub := e.Submitter("public")
	w := e.StartWorker("lifecycle", e2etest.ChildEnv(workertest.WhisperMode(workertest.SleepMode("1500ms"))))
	id := w.ID()
	principal := "worker:" + id

	// Pending: it polls its own state and does nothing else.
	first := e.Submit(sub, "public", api.JobParams{Label: "first"}, e2etest.SpeechWAV(2))
	w.WaitLog("", func(m map[string]any) bool {
		msg, _ := m["message"].(string)
		return strings.HasPrefix(msg, "worker awaiting approval") && m[obs.FieldWorkerID] == id
	})
	e.Eventually(func() bool { return len(e.Requests("/worker/{id}", principal)) >= 3 },
		"pending worker never polled its state")
	assert.Equal(t, api.WorkerPending, e.WorkerByDescription("lifecycle").State)
	assert.Empty(t, e.Requests("/worker/{id}/leases", principal), "a pending worker must not claim")
	assert.Equal(t, api.JobQueued, e.Job(first.ID).State)

	// Approved for the queue: it starts working.
	e.Approve(id, "public")
	e.WaitEvent(obs.EventTaskLeased, func(m map[string]any) bool { return m[obs.FieldJobID] == first.ID })

	// Paused mid-task: the running lease finishes, nothing new is claimed.
	e.SetWorkerState(id, "pause")
	second := e.Submit(sub, "public", api.JobParams{Label: "second"}, e2etest.SpeechWAV(2))
	e.WaitJob(first.ID, api.JobDone)
	e.Eventually(func() bool {
		for _, r := range e.Requests("/worker/{id}/leases", principal) {
			if r[obs.FieldStatus] == float64(409) {
				return true
			}
		}
		return false
	}, "the paused worker's claim was never refused with 409")
	assert.Equal(t, api.JobQueued, e.Job(second.ID).State, "a paused worker claims nothing new")
	assert.Equal(t, api.WorkerPaused, e.WorkerByDescription("lifecycle").State)

	// Resumed: it picks the second job up.
	e.SetWorkerState(id, "resume")
	e.WaitJob(second.ID, api.JobDone)

	// Rejected mid-task: the worker exits with ErrRejected and its lease
	// requeues without consuming an attempt.
	third := e.Submit(sub, "public", api.JobParams{Label: "third"}, e2etest.SpeechWAV(2))
	e.WaitEvent(obs.EventTaskLeased, func(m map[string]any) bool { return m[obs.FieldJobID] == third.ID })
	e.SetWorkerState(id, "reject")
	err := w.Wait()
	require.Error(t, err)
	assert.True(t, errors.Is(err, worker.ErrRejected), "Run returned %v", err)
	j := e.Job(third.ID)
	assert.Equal(t, api.JobRunning, j.State, "the job has started; its task waits for another worker")
	assert.Equal(t, 1, j.Tasks.Pending, "the rejected worker's lease requeued")
	assert.Equal(t, 0, j.Tasks.Leased)

	// Another worker finishes it on its first counted attempt.
	w2 := e.StartWorker("successor")
	e.Approve(w2.ID(), "public")
	e.WaitJob(third.ID, api.JobDone)
	var attempts []float64
	for _, ev := range e.Logs.Events(t, obs.EventTaskLeased) {
		if ev[obs.FieldJobID] == third.ID {
			attempts = append(attempts, ev[obs.FieldAttempt].(float64))
		}
	}
	assert.Equal(t, []float64{1, 1}, attempts, "the rejection refunded the attempt")
}

// TestPausedWorkerFinishesLeaseNeedingModel: pausing lets running leases
// finish (DESIGN.md §4.2), and a running transcribe lease may still need
// its model. Here the worker is paused after claiming but before fetching
// the model; the fetch must succeed and the task complete.
func TestPausedWorkerFinishesLeaseNeedingModel(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
	w := e.StartWorker("paused-mid-fetch")
	release := w.Net.Hold("/models/")
	defer release()
	id := w.ID()
	e.Approve(id, "public")
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	l := e.WaitEvent(obs.EventTaskLeased, func(m map[string]any) bool { return m[obs.FieldJobID] == job.ID })
	w.WaitClaimed(l[obs.FieldLeaseID])
	e.SetWorkerState(id, "pause")
	release()
	done := e.WaitJob(job.ID, api.JobDone)
	assert.Equal(t, 1, done.Tasks.Done)
	assert.Empty(t, e.Logs.Events(t, obs.EventTaskFailed))
	for _, r := range e.Requests("/models/{name}", "worker:"+id) {
		assert.Equal(t, float64(200), r[obs.FieldStatus])
	}
}
