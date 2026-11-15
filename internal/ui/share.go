package ui

import (
	"strings"
	"sync"
	"time"

	"github.com/deweysasser/whisper-network/internal/obs"
)

// Actual served share (DESIGN.md §8.2) is the scheduling cost charged to
// each queue over a recent window. The store has no lease-history query, so
// the UI derives it from the server's cumulative counter
// whisper_sched_served_cost_total{queue,class} (charged at lease time, the
// same quantity the scheduler balances): it samples the counter whenever the
// dashboard is viewed (the page polls while open) and differences the
// samples. With no sample old enough yet (just after a restart, or the first
// view in a long while) it falls back to the totals since the server started.
const (
	// shareWindow is how far back the actual share looks.
	shareWindow = time.Hour
	// shareMinSpan is the shortest difference worth showing; below it the
	// numbers are noise and the since-start totals are used instead.
	shareMinSpan = 5 * time.Minute
	// shareSpacing thins samples so a busy dashboard does not grow the ring.
	shareSpacing = 10 * time.Second
	// servedCostMetric is the counter the share is read from.
	servedCostMetric = "whisper_sched_served_cost_total"
)

// shareSample is the cumulative served cost per queue at one instant.
type shareSample struct {
	at   time.Time
	cost map[string]float64
}

// shareTracker keeps recent samples of the served-cost counter. It is safe
// for concurrent use.
type shareTracker struct {
	read func() map[string]float64

	mu      sync.Mutex
	samples []shareSample // oldest first
}

func newShareTracker(read func() map[string]float64) *shareTracker {
	return &shareTracker{read: read}
}

// observe samples the counter at now and returns the cost served per queue
// over the window, and the span it covers. A zero span means the values are
// totals since the server started.
func (t *shareTracker) observe(now time.Time) (map[string]float64, time.Duration) {
	cur := t.read()
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.samples); n == 0 || now.Sub(t.samples[n-1].at) >= shareSpacing {
		t.samples = append(t.samples, shareSample{at: now, cost: cur})
	}
	// Keep exactly one sample at or before the window start as the baseline.
	for len(t.samples) > 1 && !t.samples[1].at.After(now.Add(-shareWindow)) {
		t.samples = t.samples[1:]
	}
	base := t.samples[0]
	span := now.Sub(base.at)
	if span < shareMinSpan {
		return cur, 0
	}
	out := make(map[string]float64, len(cur))
	for q, v := range cur {
		if d := v - base.cost[q]; d > 0 {
			out[q] = d
		}
	}
	return out, span
}

// servedCostReader reads the per-queue totals of the served-cost counter
// from the server's metrics set, summing the weight classes. It only reads
// series that already exist, so it never creates metrics as a side effect.
func servedCostReader(m *obs.Metrics) func() map[string]float64 {
	return func() map[string]float64 {
		out := map[string]float64{}
		set := m.Set()
		for _, name := range set.ListMetricNames() {
			if q, ok := servedCostQueue(name); ok {
				out[q] += set.GetOrCreateFloatCounter(name).Get()
			}
		}
		return out
	}
}

// servedCostQueue extracts the queue label of a served-cost series name,
// e.g. whisper_sched_served_cost_total{queue="public",class="medium"}.
func servedCostQueue(name string) (string, bool) {
	labels, ok := strings.CutPrefix(name, servedCostMetric+"{")
	if !ok {
		return "", false
	}
	for _, kv := range strings.Split(strings.TrimSuffix(labels, "}"), ",") {
		if v, ok := strings.CutPrefix(kv, `queue="`); ok {
			return strings.TrimSuffix(v, `"`), true
		}
	}
	return "", false
}
