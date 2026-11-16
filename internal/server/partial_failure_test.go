package server

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/store"
)

func maxAttempts(n int) harnessOpt {
	return func(_ *Config, s *store.Config) { s.MaxAttempts = n }
}

// TestFailingJobOverHTTP: one task fails for good while a sibling is leased
// on another worker. The sibling's heartbeats and result upload still work,
// nothing new is leased, and job.failed is logged once, when the sibling's
// result arrives, with the job's progress in its error (#63, #65).
func TestFailingJobOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t, maxAttempts(1))
	client := h.client(submitOn("public"))
	aID, aTok := h.activeWorker(api.WorkerFacts{}, "public")
	bID, bTok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(10, media.SampleRate)))
	require.GreaterOrEqual(t, job.Tasks.Total, 3)

	la := h.claim(aID, aTok, "", nil)
	lb := h.claim(bID, bTok, "", nil)
	h.fail(la, aTok, "whisper-cli crashed")

	j := h.job(job.ID)
	assert.Equal(t, api.JobRunning, j.State)
	require.NotNil(t, j.Error)
	assert.Equal(t, 1, j.Tasks.Leased)
	assert.Equal(t, job.Tasks.Total-2, j.Tasks.Cancelled)
	assert.Nil(t, h.claim(bID, bTok, "", nil), "no new leases")
	assert.Empty(t, h.logs.events(t, obs.EventJobFailed))

	h.expect("POST", "/lease/"+lb.ID+"/heartbeat", bTok, api.HeartbeatRequest{}, http.StatusOK, nil)
	resp, b := h.putResult(lb, bTok, result(lb, "kept"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))

	j = h.job(job.ID)
	assert.Equal(t, api.JobFailed, j.State)
	assert.Equal(t, api.TaskCounts{Total: job.Tasks.Total, Done: 1, Failed: 1, Cancelled: job.Tasks.Total - 2}, j.Tasks)
	want := "transcribe task 1 failed after 1 attempt(s) (on worker " + aID + "): whisper-cli crashed; 1 of " +
		strconv.Itoa(job.Tasks.Total) + " tasks done, " + strconv.Itoa(job.Tasks.Total-2) + " cancelled"
	assert.Equal(t, want, *j.Error)
	jf := h.logs.events(t, obs.EventJobFailed)
	require.Len(t, jf, 1)
	assert.Equal(t, want, jf[0][obs.FieldError])
}

// TestPreWorkFailureOverHTTP: a worker reporting pre-work failures gets the
// task back each time, is paused by the breaker, and the job never fails;
// an unknown stage counts as none (#64).
func TestPreWorkFailureOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t, maxAttempts(1), failureLimit(2))
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))

	for i := range 2 {
		l := h.claim(wid, wtok, "", nil)
		require.NotNil(t, l, "claim %d", i)
		h.expect("POST", "/lease/"+l.ID+"/failure", wtok, api.FailureReport{Reason: "whisper-cli is not installed",
			Retryable: true, Stage: api.StagePreWork}, http.StatusNoContent, nil)
	}
	require.Len(t, h.logs.events(t, obs.EventWorkerAutoPaused), 1)
	failed := h.logs.events(t, obs.EventTaskFailed)
	require.Len(t, failed, 2)
	assert.Equal(t, "pre-work", failed[0]["stage"])
	j := h.job(job.ID)
	assert.Equal(t, api.JobRunning, j.State)
	assert.Nil(t, j.Error)
	assert.Equal(t, 1, j.Tasks.Pending)

	// An unknown stage is no stage: the failure counts and fails the job.
	w2, tok2 := h.activeWorker(api.WorkerFacts{}, "public")
	l := h.claim(w2, tok2, "", nil)
	h.expect("POST", "/lease/"+l.ID+"/failure", tok2, api.FailureReport{Reason: "boom", Retryable: true,
		Stage: "later"}, http.StatusNoContent, nil)
	assert.Equal(t, api.JobFailed, h.job(job.ID).State)
}
