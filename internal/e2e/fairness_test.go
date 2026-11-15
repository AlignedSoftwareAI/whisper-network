package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/e2e/e2etest"
	"github.com/deweysasser/whisper-network/internal/obs"
)

// servedOrder waits for n task.leased events and returns field of each,
// in lease order (one worker with one slot leases strictly in sequence).
// The order is decided by the server alone, so the result does not depend
// on timing; the deadline is only a safety net, generous because n tasks run
// one after another and each starts two processes (slow under -race and
// load).
func servedOrder(t *testing.T, e *e2etest.Env, n int, field string) []string {
	t.Helper()
	deadline := time.Now().Add(servedOrderTimeout)
	for {
		evs := e.Logs.Events(t, obs.EventTaskLeased)
		if len(evs) >= n {
			out := make([]string, n)
			for i, ev := range evs[:n] {
				out[i] = ev[field].(string)
			}
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d tasks were leased within %s", len(evs), n, servedOrderTimeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// servedOrderTimeout bounds servedOrder.
const servedOrderTimeout = 5 * time.Minute

func count(xs []string) map[string]int {
	m := map[string]int{}
	for _, x := range xs {
		m[x]++
	}
	return m
}

// TestWeightedFairnessAcrossQueues (scenario 7): two backlogged queues with
// weights 100 and 300 served by a single one-slot worker share it 1:3.
func TestWeightedFairnessAcrossQueues(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
	e.CreateQueue("light", 100)
	e.CreateQueue("heavy", 300)
	sub := e.Submitter("light", "heavy")
	clip := e2etest.SpeechWAV(1) // one task of equal cost per job
	for range 12 {
		e.Submit(sub, "light", api.JobParams{}, clip)
	}
	for range 36 {
		e.Submit(sub, "heavy", api.JobParams{}, clip)
	}
	w := e.StartWorker("solo")
	e.Approve(w.ID(), "light", "heavy")

	got := count(servedOrder(t, e, 24, obs.FieldQueue))
	t.Logf("first 24 leases by queue: %v", got)
	// Start-time fair queueing keeps the deviation within one task.
	assert.InDelta(t, 6, got["light"], 1, "served %v", got)
	assert.InDelta(t, 18, got["heavy"], 1, "served %v", got)
}

// TestWeightClassesWithinQueue (scenario 7): low, medium and high jobs in one
// backlogged queue share the worker 1:2:4.
func TestWeightClassesWithinQueue(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
	sub := e.Submitter("public")
	clip := e2etest.SpeechWAV(1)
	for range 12 {
		for _, wt := range []api.Weight{api.WeightLow, api.WeightMedium, api.WeightHigh} {
			e.Submit(sub, "public", api.JobParams{Weight: wt}, clip)
		}
	}
	w := e.StartWorker("solo")
	e.Approve(w.ID(), "public")

	got := count(servedOrder(t, e, 14, obs.FieldWeight))
	t.Logf("first 14 leases by class: %v", got)
	assert.InDelta(t, 2, got["low"], 1, "served %v", got)
	assert.InDelta(t, 4, got["medium"], 1, "served %v", got)
	assert.InDelta(t, 8, got["high"], 1, "served %v", got)
	assert.Positive(t, got["low"], "low is slowed, never starved")
}
