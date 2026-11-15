package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/store"
)

func failureLimit(n int) harnessOpt {
	return func(_ *Config, s *store.Config) { s.WorkerFailureLimit = n }
}

// TestWorkerCircuitBreaker: a worker that fails three tasks in a row across
// different jobs is paused by the system: audited, logged as
// worker.auto_paused, counted and exported, and shown with its reason. It
// claims nothing more; a resume clears the streak and the reason.
func TestWorkerCircuitBreaker(t *testing.T) {
	t.Parallel()
	h := newHarness(t, failureLimit(3))
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	for range 4 {
		h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	}
	for i := range 3 {
		assert.Empty(t, h.logs.events(t, obs.EventWorkerAutoPaused), "not before the third failure")
		l := h.claim(wid, wtok, "", nil)
		require.NotNil(t, l, "claim %d", i)
		h.fail(l, wtok, "whisper-cli crashed")
	}

	evs := h.logs.events(t, obs.EventWorkerAutoPaused)
	require.Len(t, evs, 1)
	assert.Equal(t, "warn", evs[0]["level"])
	assert.Equal(t, wid, evs[0][obs.FieldWorkerID])
	assert.Equal(t, store.SystemActor, evs[0][obs.FieldActor])
	assert.Equal(t, "3 consecutive failures across 3 jobs", evs[0][obs.FieldReason])
	assert.EqualValues(t, 3, evs[0][obs.FieldFailures])
	assert.EqualValues(t, 3, evs[0][obs.FieldJobs])

	es, err := h.st.QueryAudit(h.ctx, store.AuditFilter{ActionPrefix: "worker.pause", Target: store.WorkerPrincipal(wid)})
	require.NoError(t, err)
	require.Len(t, es, 1)
	assert.Equal(t, store.SystemActor, es[0].Actor)
	assert.Contains(t, es[0].Detail, `"reason":"3 consecutive failures across 3 jobs"`)

	ws := h.workerSummary(wid)
	assert.Equal(t, api.WorkerPaused, ws.State)
	assert.Equal(t, store.SystemActor, ws.DecidedBy)
	assert.Equal(t, "3 consecutive failures across 3 jobs", ws.PauseReason)
	assert.Equal(t, 3, ws.FailureStreak)
	assert.Equal(t, 3, ws.FailureStreakJobs)

	m := h.scrape()
	assert.Contains(t, m, `whisper_worker_auto_paused_total{worker="`+wid+`"} 1`)
	assert.Contains(t, m, `whisper_worker_auto_paused{worker="`+wid+`"} 1`)
	assert.Contains(t, m, `whisper_worker_failure_streak{worker="`+wid+`"} 3`)

	resp, body := h.call("POST", "/worker/"+wid+"/leases", wtok, api.ClaimRequest{})
	h.expectProblem(resp, body, http.StatusConflict, api.ProblemWorkerPaused)

	h.expect("POST", "/admin/workers/"+wid+"/resume", h.admin, nil, http.StatusNoContent, nil)
	ws = h.workerSummary(wid)
	assert.Equal(t, api.WorkerActive, ws.State)
	assert.Empty(t, ws.PauseReason)
	assert.Zero(t, ws.FailureStreak)
	m = h.scrape()
	assert.Contains(t, m, `whisper_worker_auto_paused{worker="`+wid+`"} 0`)
	assert.Contains(t, m, `whisper_worker_failure_streak{worker="`+wid+`"} 0`)
	require.NotNil(t, h.claim(wid, wtok, "", nil), "resumed: it claims again")

	var raw []map[string]any
	require.NoError(t, json.Unmarshal(h.expect("GET", "/admin/workers", h.admin, nil, http.StatusOK, nil), &raw))
	assert.NotContains(t, raw[0], "pause_reason", "omitted when not auto-paused")
	assert.Contains(t, raw[0], "failure_streak")
}

// TestWorkerCircuitBreakerPausedByAdmin: an administrator's pause has no
// reason and is not an auto-pause.
func TestWorkerCircuitBreakerPausedByAdmin(t *testing.T) {
	t.Parallel()
	h := newHarness(t, failureLimit(3))
	wid, _ := h.activeWorker(api.WorkerFacts{}, "public")
	h.expect("POST", "/admin/workers/"+wid+"/pause", h.admin, nil, http.StatusNoContent, nil)
	ws := h.workerSummary(wid)
	assert.Equal(t, api.WorkerPaused, ws.State)
	assert.Empty(t, ws.PauseReason)
	assert.Contains(t, h.scrape(), `whisper_worker_auto_paused{worker="`+wid+`"} 0`)
}
