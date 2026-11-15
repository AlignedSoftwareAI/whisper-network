package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/e2e/e2etest"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
)

// breaker turns on the worker circuit breaker at the server's default
// limit of 3. A worker never gets back a task it failed, so the broken
// worker needs three jobs to fail three tasks.
func breaker(_ *server.Config, s *store.Config) {
	s.WorkerFailureLimit = 3
}

// TestWorkerCircuitBreaker: a worker whose whisper-cli always crashes is
// paused by the server after three failures across three jobs, and the
// jobs complete on a healthy worker.
func TestWorkerCircuitBreaker(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t, breaker)
	sub := e.Submitter("public")
	a := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	b := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	c := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	broken := e.StartWorker("broken", crash)
	e.Approve(broken.ID(), "public")

	ev := e.WaitEvent(obs.EventWorkerAutoPaused, nil)
	assert.Equal(t, broken.ID(), ev[obs.FieldWorkerID])
	assert.Equal(t, "3 consecutive failures across 3 jobs", ev[obs.FieldReason])
	ws := e.WorkerByDescription("broken")
	assert.Equal(t, api.WorkerPaused, ws.State)
	assert.Equal(t, "3 consecutive failures across 3 jobs", ws.PauseReason)
	assert.Equal(t, "system", ws.DecidedBy)
	var audit api.AuditList
	e.AdminJSON(http.MethodGet, "/admin/audit?action=worker.pause", nil, http.StatusOK, &audit)
	require.Len(t, audit.Entries, 1)
	assert.Equal(t, "system", audit.Entries[0].Actor)
	assert.Contains(t, e.Scrape(), `whisper_worker_auto_paused_total{worker="`+broken.ID()+`"} 1`)

	healthy := e.StartWorker("healthy")
	e.Approve(healthy.ID(), "public")
	e.WaitJob(a.ID, api.JobDone)
	e.WaitJob(b.ID, api.JobDone)
	e.WaitJob(c.ID, api.JobDone)
	failed := len(e.Logs.Events(t, obs.EventTaskFailed))
	assert.Equal(t, 3, failed, "the broken worker took nothing after it was paused")
	assert.Equal(t, api.WorkerPaused, e.WorkerByDescription("broken").State, "it stays paused until resumed")
}
