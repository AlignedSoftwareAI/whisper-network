package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/store"
)

// TestAdminWorkersStats: GET /admin/workers carries each worker's counters,
// updated by the result it uploaded.
func TestAdminWorkersStats(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	idle, _ := h.activeWorker(api.WorkerFacts{Hostname: "idle"}, "public")
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	require.NotNil(t, l)
	resp, b := h.putResult(l, wtok, result(l, "hello"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))

	raw := h.expect("GET", "/admin/workers", h.admin, nil, http.StatusOK, nil)
	var ws []api.WorkerSummary
	require.NoError(t, json.Unmarshal(raw, &ws))
	byID := map[string]api.WorkerSummary{}
	for _, w := range ws {
		byID[w.ID] = w
	}
	require.NotNil(t, byID[wid].Stats)
	now := h.clk.Now()
	assert.Equal(t, api.WorkerStats{TranscribeTasks: 1, Jobs: 1, AudioS: 2, ComputeS: 1.5, TranscribeComputeS: 1.5,
		FirstTaskAt: &now, LastTaskAt: &now}, *byID[wid].Stats)
	require.NotNil(t, byID[idle].Stats, "a worker that has done nothing reports zeros")
	assert.Equal(t, api.WorkerStats{}, *byID[idle].Stats)

	var docs []map[string]any
	require.NoError(t, json.Unmarshal(raw, &docs))
	stats, ok := docs[0]["stats"].(map[string]any)
	require.True(t, ok, "stats is an object in %s", raw)
	for _, k := range []string{"prepare_tasks", "transcribe_tasks", "jobs", "audio_s", "compute_s",
		"transcribe_compute_s", "failures", "expired_leases"} {
		assert.Contains(t, stats, k)
	}
}

// TestPruneRejectedWorkerAuditsStats: the counters of a pruned worker are
// deleted with it, and kept in its worker.prune audit entry.
func TestPruneRejectedWorkerAuditsStats(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	idle, _ := h.worker(api.WorkerFacts{Hostname: "idle"})
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	require.NotNil(t, l)
	resp, b := h.putResult(l, wtok, result(l, "hello"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	for _, id := range []string{wid, idle} {
		h.expect("POST", "/admin/workers/"+id+"/reject", h.admin, nil, http.StatusNoContent, nil)
	}
	h.clk.Advance(h.srv.cfg.RejectedWorkerRetention + time.Second)

	n, err := h.srv.Ops().PruneRejectedWorkers(h.ctx, h.clk.Now().Add(-h.srv.cfg.RejectedWorkerRetention))
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)

	es, err := h.st.QueryAudit(h.ctx, store.AuditFilter{ActionPrefix: "worker.prune"})
	require.NoError(t, err)
	details := map[string]map[string]any{}
	for _, e := range es {
		var d map[string]any
		require.NoError(t, json.Unmarshal([]byte(e.Detail), &d))
		details[e.Target] = d
	}
	worked := details[store.WorkerPrincipal(wid)]
	require.Contains(t, worked, "stats")
	assert.EqualValues(t, 1, worked["stats"].(map[string]any)["transcribe_tasks"])
	assert.NotContains(t, details[store.WorkerPrincipal(idle)], "stats", "nothing to keep for an idle worker")
}
