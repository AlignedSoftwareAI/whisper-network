package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

func f64(v float64) *float64 { return &v }

// A worker whose host is too busy to claim reports its gauges on their own;
// they reach the admin API and the metrics, and keep the worker seen.
func TestGaugeReportRecordsHostLoad(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	assert.NotContains(t, h.scrape(), `whisper_worker_host_load{worker="`+wid+`"}`, "nothing reported yet")

	g := api.Gauges{HostLoad: f64(85.5), DeferredForLoad: true, ClaimsDeferred: i64(3)}
	h.expect("POST", "/worker/"+wid+"/gauges", wtok, g, http.StatusNoContent, nil)
	w := h.workerSummary(wid)
	require.NotNil(t, w.Gauges)
	assert.Equal(t, g.HostLoad, w.Gauges.HostLoad)
	assert.True(t, w.Gauges.DeferredForLoad)
	require.NotNil(t, w.LastSeenAt)

	m := h.scrape()
	assert.Contains(t, m, `whisper_worker_host_load{worker="`+wid+`"} 85.5`)
	assert.Contains(t, m, `whisper_worker_claims_deferred_total{worker="`+wid+`"} 3`)

	// The next claim says the worker is taking work again.
	assert.Nil(t, h.claim(wid, wtok, "0s", &api.ClaimRequest{Kinds: []api.TaskKind{api.KindTranscribe},
		Gauges: &api.Gauges{HostLoad: f64(20), ClaimsDeferred: i64(3)}}))
	w = h.workerSummary(wid)
	assert.False(t, w.Gauges.DeferredForLoad)
	assert.Equal(t, f64(20), w.Gauges.HostLoad)

	// Out-of-range readings are dropped, not stored.
	h.expect("POST", "/worker/"+wid+"/gauges", wtok, api.Gauges{HostLoad: f64(250), ClaimsDeferred: i64(-1)},
		http.StatusNoContent, nil)
	w = h.workerSummary(wid)
	assert.Nil(t, w.Gauges.HostLoad)
	assert.Nil(t, w.Gauges.ClaimsDeferred)
}

// Only the worker itself may report, and only while active or paused.
func TestGaugeReportAuthz(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	_, otherTok := h.activeWorker(api.WorkerFacts{}, "public")
	g := api.Gauges{HostLoad: f64(90)}
	resp, _ := h.call("POST", "/worker/"+wid+"/gauges", "", g)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp, _ = h.call("POST", "/worker/"+wid+"/gauges", otherTok, g)
	assert.NotEqual(t, http.StatusNoContent, resp.StatusCode, "another worker")
	resp, _ = h.call("POST", "/worker/"+wid+"/gauges", h.admin, g)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "not a worker")
	assert.Nil(t, h.workerSummary(wid).Gauges)

	_, err := h.st.SetWorkerState(h.ctx, wid, api.WorkerPaused, "user:1")
	require.NoError(t, err)
	h.expect("POST", "/worker/"+wid+"/gauges", wtok, g, http.StatusNoContent, nil)
	_, err = h.st.SetWorkerState(h.ctx, wid, api.WorkerRejected, "user:1")
	require.NoError(t, err)
	resp, _ = h.call("POST", "/worker/"+wid+"/gauges", wtok, g)
	assert.Equal(t, http.StatusGone, resp.StatusCode)
}
