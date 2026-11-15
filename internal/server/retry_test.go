package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/store"
)

func (h *harness) fail(l *api.Lease, token, reason string) {
	h.t.Helper()
	h.expect("POST", "/lease/"+l.ID+"/failure", token, api.FailureReport{Reason: reason, Retryable: true},
		http.StatusNoContent, nil)
}

// TestRetryElsewhere: a task that failed on worker A goes to worker B, never
// back to A, and the failed job names both workers.
func TestRetryElsewhere(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	aID, aTok := h.activeWorker(api.WorkerFacts{}, "public")
	bID, bTok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))

	l := h.claim(aID, aTok, "", nil)
	require.NotNil(t, l)
	h.fail(l, aTok, "model file corrupt")
	assert.Nil(t, h.claim(aID, aTok, "", nil), "A failed the task; it waits for another worker")
	h.clk.Advance(12 * time.Hour)
	assert.Nil(t, h.claim(aID, aTok, "", nil), "however long it waits")
	l = h.claim(bID, bTok, "", nil)
	require.NotNil(t, l, "B gets the retry")
	h.fail(l, bTok, "out of memory")

	j := h.job(job.ID)
	require.Equal(t, api.JobFailed, j.State)
	require.NotNil(t, j.Error)
	assert.Equal(t, "transcribe task 1 failed after 2 attempt(s) (on workers "+aID+", "+bID+"): out of memory", *j.Error)
}

// TestRetryWaitLimit: with A the only eligible worker, the task it failed
// waits; the reaper fails the job once it has waited --retry-wait-limit,
// naming A, and logs task.failed and job.failed.
func TestRetryWaitLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) { c.RetryWaitLimit = time.Hour })
	client := h.client(submitOn("public"))
	aID, aTok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))

	l := h.claim(aID, aTok, "", nil)
	require.NotNil(t, l)
	h.fail(l, aTok, "flaky disk")
	h.clk.Advance(time.Hour - time.Second)
	h.srv.reapOnce(h.ctx)
	assert.Nil(t, h.claim(aID, aTok, "", nil))
	assert.Equal(t, api.JobRunning, h.job(job.ID).State, "still waiting for another worker")

	h.clk.Advance(time.Second)
	h.srv.reapOnce(h.ctx)
	j := h.job(job.ID)
	require.Equal(t, api.JobFailed, j.State)
	require.NotNil(t, j.Error)
	want := "transcribe task 1 failed after 1 attempt(s) (on worker " + aID +
		"); no other eligible worker took it within 1h: flaky disk"
	assert.Equal(t, want, *j.Error)

	failed := h.logs.events(t, obs.EventTaskFailed)
	require.Len(t, failed, 2)
	assert.Equal(t, want, failed[1][obs.FieldError])
	assert.Equal(t, []any{aID}, failed[1]["failed_on"])
	jf := h.logs.events(t, obs.EventJobFailed)
	require.Len(t, jf, 1)
	assert.Equal(t, job.ID, jf[0][obs.FieldJobID])
}
