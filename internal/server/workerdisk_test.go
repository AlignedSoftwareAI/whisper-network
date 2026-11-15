package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/media"
)

func i64(v int64) *int64 { return &v }

func (h *harness) workerSummary(id string) api.WorkerSummary {
	h.t.Helper()
	var ws []api.WorkerSummary
	h.expect("GET", "/admin/workers", h.admin, nil, http.StatusOK, &ws)
	for _, w := range ws {
		if w.ID == id {
			return w
		}
	}
	h.t.Fatalf("worker %s not listed", id)
	return api.WorkerSummary{}
}

func (h *harness) scrape() string {
	h.t.Helper()
	h.srv.refreshStats(h.ctx)
	var mb bytes.Buffer
	h.metrics.WritePrometheus(&mb)
	return mb.String()
}

// An idle worker sends no heartbeats; its claims carry the disk gauges, which
// reach the admin API and the metrics.
func TestClaimRecordsDiskGauges(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")

	assert.NotContains(t, h.scrape(), `whisper_worker_state_free_bytes{worker="`+wid+`"}`,
		"nothing reported yet, so no series (not a zero that would alert)")

	g := &api.Gauges{ModelsBytes: i64(3 << 30), WorkBytes: i64(0), StateFreeBytes: i64(20 << 30),
		CachedModels: []api.CachedModel{{Name: "small.en", Bytes: 3 << 30}}}
	assert.Nil(t, h.claim(wid, wtok, "0s", &api.ClaimRequest{Kinds: []api.TaskKind{api.KindTranscribe},
		Models: []string{"small.en"}, Gauges: g}))

	w := h.workerSummary(wid)
	require.NotNil(t, w.Gauges)
	assert.Equal(t, g.ModelsBytes, w.Gauges.ModelsBytes)
	assert.Equal(t, g.WorkBytes, w.Gauges.WorkBytes)
	assert.Equal(t, g.StateFreeBytes, w.Gauges.StateFreeBytes)
	assert.Equal(t, g.CachedModels, w.Gauges.CachedModels)

	m := h.scrape()
	for _, want := range []string{
		`whisper_worker_models_bytes{worker="` + wid + `"} 3221225472`,
		`whisper_worker_work_bytes{worker="` + wid + `"} 0`,
		`whisper_worker_state_free_bytes{worker="` + wid + `"} 21474836480`,
	} {
		assert.Contains(t, m, want)
	}

	// A claim without gauges (an older worker) keeps the last readings.
	assert.Nil(t, h.claim(wid, wtok, "0s", &api.ClaimRequest{Kinds: []api.TaskKind{api.KindTranscribe}}))
	assert.Equal(t, g.StateFreeBytes, h.workerSummary(wid).Gauges.StateFreeBytes)
}

// Heartbeats carry the same disk gauges; nonsense values are dropped rather
// than failing the heartbeat.
func TestHeartbeatDiskGaugesSanitized(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	require.NotNil(t, l)

	tooMany := make([]api.CachedModel, api.MaxCachedModels+1)
	for i := range tooMany {
		tooMany[i] = api.CachedModel{Name: "m", Bytes: 1}
	}
	h.expect("POST", "/lease/"+l.ID+"/heartbeat", wtok, api.HeartbeatRequest{Gauges: &api.Gauges{
		ModelsBytes: i64(-1), WorkBytes: i64(7), StateFreeBytes: i64(0), CachedModels: tooMany,
	}}, http.StatusOK, nil)
	g := h.workerSummary(wid).Gauges
	require.NotNil(t, g)
	assert.Nil(t, g.ModelsBytes, "negative reading dropped")
	assert.Equal(t, i64(7), g.WorkBytes)
	assert.Equal(t, i64(0), g.StateFreeBytes, "a full disk is reported as zero, not dropped")
	assert.Nil(t, g.CachedModels, "oversized list dropped")

	h.expect("POST", "/lease/"+l.ID+"/heartbeat", wtok, api.HeartbeatRequest{Gauges: &api.Gauges{
		CachedModels: []api.CachedModel{{Name: strings.Repeat("x", 101), Bytes: 1}},
	}}, http.StatusOK, nil)
	assert.Nil(t, h.workerSummary(wid).Gauges.CachedModels, "overlong name drops the list")
}
