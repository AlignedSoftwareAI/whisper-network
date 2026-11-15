package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/e2e/e2etest"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

func retryGrace(g time.Duration) e2etest.Option {
	return func(_ *server.Config, s *store.Config) { s.RetryElsewhereGrace = g }
}

var crash = e2etest.ChildEnv(workertest.WhisperMode(workertest.ModeCrash))

// TestRetryGoesElsewhere: a task that crashed on one worker is retried by
// another, though the broken worker keeps polling the whole time.
func TestRetryGoesElsewhere(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t, retryGrace(time.Hour))
	sub := e.Submitter("public")
	broken := e.StartWorker("broken", crash)
	e.Approve(broken.ID(), "public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	e.WaitEvent(obs.EventTaskFailed, func(m map[string]any) bool { return m[obs.FieldJobID] == job.ID })

	healthy := e.StartWorker("healthy")
	e.Approve(healthy.ID(), "public")
	e.WaitJob(job.ID, api.JobDone)
	ls := leasedFor(t, e, job.ID)
	require.Len(t, ls, 2)
	assert.Equal(t, broken.ID(), ls[0][obs.FieldWorkerID])
	assert.Equal(t, healthy.ID(), ls[1][obs.FieldWorkerID])
	assert.Nil(t, ls[1][obs.FieldReason])
}

// TestRetrySameWorkerAfterGrace: with only one eligible worker, it retries
// the task it failed once the grace has passed, rather than the task
// waiting forever; the lease event and the job error say what happened.
func TestRetrySameWorkerAfterGrace(t *testing.T) {
	t.Parallel()
	const g = time.Second
	e := e2etest.New(t, retryGrace(g))
	w := e.StartWorker("alone", crash)
	e.Approve(w.ID(), "public")
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	failed := e.WaitJob(job.ID, api.JobFailed)
	require.NotNil(t, failed.Error)
	assert.Contains(t, *failed.Error, "failed after 2 attempt(s) (on workers "+w.ID()+", "+w.ID()+"): ")

	ls := leasedFor(t, e, job.ID)
	require.Len(t, ls, 2)
	assert.Nil(t, ls[0][obs.FieldReason])
	assert.Contains(t, ls[1][obs.FieldReason], "retry on the worker that already failed this task")
	first, err := time.Parse(time.RFC3339Nano, e.Logs.Events(t, obs.EventTaskFailed)[0]["time"].(string))
	require.NoError(t, err)
	second, err := time.Parse(time.RFC3339Nano, ls[1]["time"].(string))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, second.Sub(first), g-time.Second/10, "not retried within the grace")
}
