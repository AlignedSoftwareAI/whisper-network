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

func retryGrace(g time.Duration) harnessOpt {
	return func(_ *Config, s *store.Config) { s.RetryElsewhereGrace = g }
}

func (h *harness) fail(l *api.Lease, token, reason string) {
	h.t.Helper()
	h.expect("POST", "/lease/"+l.ID+"/failure", token, api.FailureReport{Reason: reason, Retryable: true},
		http.StatusNoContent, nil)
}

// TestRetryElsewhere: a task that failed on worker A goes to worker B, never
// back to A, and the failed job names both workers.
func TestRetryElsewhere(t *testing.T) {
	t.Parallel()
	h := newHarness(t, retryGrace(10*time.Minute))
	client := h.client(submitOn("public"))
	aID, aTok := h.activeWorker(api.WorkerFacts{}, "public")
	bID, bTok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))

	l := h.claim(aID, aTok, "", nil)
	require.NotNil(t, l)
	h.fail(l, aTok, "model file corrupt")
	assert.Nil(t, h.claim(aID, aTok, "", nil), "A failed the task; it waits for another worker")
	l = h.claim(bID, bTok, "", nil)
	require.NotNil(t, l, "B gets the retry")
	h.fail(l, bTok, "out of memory")

	j := h.job(job.ID)
	require.Equal(t, api.JobFailed, j.State)
	require.NotNil(t, j.Error)
	assert.Equal(t, "transcribe task 1 failed after 2 attempt(s) (on workers "+aID+", "+bID+"): out of memory", *j.Error)
	for _, ev := range h.logs.events(t, obs.EventTaskLeased) {
		assert.Nil(t, ev[obs.FieldReason], "no same-worker retry")
	}
}

// TestRetryElsewhereOnlyWorker: with no other worker, A gets the task back
// once the grace has passed, and the task.leased event says why.
func TestRetryElsewhereOnlyWorker(t *testing.T) {
	t.Parallel()
	h := newHarness(t, retryGrace(10*time.Minute))
	client := h.client(submitOn("public"))
	aID, aTok := h.activeWorker(api.WorkerFacts{}, "public")
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))

	l := h.claim(aID, aTok, "", nil)
	require.NotNil(t, l)
	h.fail(l, aTok, "flaky disk")
	h.clk.Advance(10*time.Minute - time.Second)
	assert.Nil(t, h.claim(aID, aTok, "", nil))
	h.clk.Advance(time.Second)
	again := h.claim(aID, aTok, "", nil)
	require.NotNil(t, again, "the grace has passed")
	assert.Equal(t, l.Seq, again.Seq)

	evs := h.logs.events(t, obs.EventTaskLeased)
	require.Len(t, evs, 2)
	assert.Nil(t, evs[0][obs.FieldReason])
	assert.Equal(t, aID, evs[1][obs.FieldWorkerID])
	assert.Contains(t, evs[1][obs.FieldReason], "retry on the worker that already failed this task")
}
