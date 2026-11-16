package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

// whisper_fleet_slots exposes the dashboard's capacity numbers (#81): slots
// in use, available and deferred per kind, and paused slots.
func TestFleetSlotsMetric(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	both := []api.TaskKind{api.KindPrepare, api.KindTranscribe}
	busy, busyTok := h.activeWorker(api.WorkerFacts{Slots: 4, Kinds: both, SandboxLevel: api.SandboxFull}, "public")
	deferring, deferTok := h.activeWorker(api.WorkerFacts{Slots: 2, Kinds: []api.TaskKind{api.KindTranscribe}}, "public")
	paused, _ := h.activeWorker(api.WorkerFacts{Slots: 3, Kinds: both}, "public")
	h.expect("POST", "/worker/"+busy+"/gauges", busyTok, api.Gauges{SlotsBusy: 3}, http.StatusNoContent, nil)
	h.expect("POST", "/worker/"+deferring+"/gauges", deferTok, api.Gauges{DeferredForLoad: true},
		http.StatusNoContent, nil)
	_, err := h.st.SetWorkerState(h.ctx, paused, api.WorkerPaused, "user:1")
	require.NoError(t, err)

	m := h.scrape()
	for _, want := range []string{
		`whisper_fleet_slots{kind="all",state="in_use"} 3`,
		`whisper_fleet_slots{kind="all",state="available"} 1`,
		`whisper_fleet_slots{kind="all",state="deferred"} 2`,
		`whisper_fleet_slots{kind="all",state="paused"} 3`,
		`whisper_fleet_slots{kind="prepare",state="in_use"} 3`,
		`whisper_fleet_slots{kind="prepare",state="available"} 1`,
		`whisper_fleet_slots{kind="transcribe",state="deferred"} 2`,
	} {
		assert.Contains(t, m, want)
	}
}
