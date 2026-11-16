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

var crash = e2etest.ChildEnv(workertest.WhisperMode(workertest.ModeCrash))

// TestRetryGoesElsewhere: a task that crashed on one worker is retried by
// another, though the broken worker keeps polling the whole time.
func TestRetryGoesElsewhere(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
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
}

// TestRetryWaitLimit: a task that failed on the only eligible worker is
// never retried there; once it has waited --retry-wait-limit the job fails,
// naming the worker.
func TestRetryWaitLimit(t *testing.T) {
	t.Parallel()
	const limit = 3 * time.Second
	e := e2etest.New(t, func(c *server.Config, _ *store.Config) { c.RetryWaitLimit = limit })
	w := e.StartWorker("alone", crash)
	e.Approve(w.ID(), "public")
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	failed := e.WaitJob(job.ID, api.JobFailed)
	require.NotNil(t, failed.Error)
	// The attempt normally ends with the crash report; on a heavily loaded
	// machine its lease may expire first (#43). Either way it counts once.
	assert.Contains(t, *failed.Error, "transcribe task 1 failed after 1 attempt(s) (on worker "+w.ID()+
		"); no other eligible worker took it within 3s: ")
	assert.Len(t, leasedFor(t, e, job.ID), 1, "never retried on the worker that failed it")

	reaped := e.WaitEvent(obs.EventTaskFailed, func(m map[string]any) bool { return m["failed_on"] != nil })
	assert.Equal(t, job.ID, reaped[obs.FieldJobID])
	assert.Equal(t, []any{w.ID()}, reaped["failed_on"])
	assert.Equal(t, *failed.Error, reaped[obs.FieldError].(string)+"; 0 of 1 tasks done, 0 cancelled")

	// The wait runs from the end of the attempt (crash report or expiry).
	var ended time.Time
	for _, ev := range append(e.Logs.Events(t, obs.EventTaskFailed), e.Logs.Events(t, obs.EventLeaseExpired)...) {
		if ev[obs.FieldJobID] != job.ID || ev["failed_on"] != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, ev["time"].(string))
		require.NoError(t, err)
		if ended.IsZero() || at.Before(ended) {
			ended = at
		}
	}
	require.False(t, ended.IsZero(), "the attempt's end is logged")
	at, err := time.Parse(time.RFC3339Nano, reaped["time"].(string))
	require.NoError(t, err)
	// Log timestamps have one-second resolution.
	assert.GreaterOrEqual(t, at.Sub(ended), limit-time.Second, "failed only at the limit")
}
