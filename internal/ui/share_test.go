package ui

import (
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/server"
)

// TestServedCostReader reads the real counter the scheduler feeds, so a
// change to the metric's name or labels breaks this test, not the page.
func TestServedCostReader(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics(metrics.NewSet())
	m.ServedCost("public", api.WeightMedium, 10)
	m.ServedCost("public", api.WeightLow, 5)
	m.ServedCost("meetings-2", api.WeightHigh, 30)
	m.JobCreated("public", api.WeightMedium, true) // an unrelated series
	read := servedCostReader(m)
	assert.Equal(t, map[string]float64{"public": 15, "meetings-2": 30}, read())
	before := len(m.Set().ListMetricNames())
	read()
	assert.Len(t, m.Set().ListMetricNames(), before, "reading creates no series")
}

func TestShareTrackerWindow(t *testing.T) {
	t.Parallel()
	total := map[string]float64{}
	tr := newShareTracker(func() map[string]float64 {
		out := map[string]float64{}
		for k, v := range total {
			out[k] = v
		}
		return out
	})
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	total["a"] = 100 // served before the dashboard was first opened
	got, span := tr.observe(t0)
	assert.Zero(t, span, "no history yet: totals since start")
	assert.Equal(t, map[string]float64{"a": 100}, got)

	total["a"], total["b"] = 130, 90
	got, span = tr.observe(t0.Add(10 * time.Minute))
	assert.Equal(t, 10*time.Minute, span)
	assert.Equal(t, map[string]float64{"a": 30, "b": 90}, got)

	// Samples older than the window stop counting; one baseline remains.
	for i := 1; i <= 12; i++ {
		total["b"] += 10
		tr.observe(t0.Add(10*time.Minute + time.Duration(i)*5*time.Minute))
	}
	now := t0.Add(10*time.Minute + 60*time.Minute)
	got, span = tr.observe(now)
	assert.Equal(t, shareWindow, span)
	assert.Equal(t, map[string]float64{"b": 120}, got, "a served nothing in the last hour")
	tr.mu.Lock()
	n := len(tr.samples)
	tr.mu.Unlock()
	assert.LessOrEqual(t, n, 14)

	// Views closer together than shareSpacing do not add samples.
	tr.observe(now.Add(time.Second))
	tr.mu.Lock()
	assert.Len(t, tr.samples, n)
	tr.mu.Unlock()
}

// TestDashboardNumbers runs a job to completion and checks the dashboard
// shows the throughput and the served share it produced.
func TestDashboardNumbers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.queue("idle")
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	tok := e.client(api.Grant{Role: api.RoleSubmit, Scope: "queue:public"})
	for range 3 {
		e.submitAudio(tok, "public", "x", 12)
	}
	wid, wtok := e.worker("runner", "")
	require.NoError(t, e.srv.Ops().ApproveWorker(e.ctx, server.CLIActor(), wid, []string{"public"}))

	b := e.login("root")
	_, body := b.get("/ui/")
	body = plainHelp(body)
	assert.Regexp(t, `<span class="big">3</span><span>pending tasks</span>`, body)

	e.runJob(wid, wtok)
	resp, body := b.get("/ui/dashboard/stats")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body = plainHelp(body)
	// 3 jobs × 12 s of audio in the last hour: 0.01 h/h.
	assert.Contains(t, body, `<span class="big">0.01</span><span>audio-hours / hour</span>`)
	assert.Contains(t, body, "since the server started")
	share := regexp.MustCompile(`<td>public</td>\s*<td class="n">100</td>\s*<td><meter[^>]*value="100.0"></meter> 100%</td>\s*<td><meter[^>]*value="100.0"`)
	assert.Regexp(t, share, body, "public was served everything, and is the only queue with work")
	assert.Regexp(t, `<td>idle</td>\s*<td class="n">100</td>\s*<td><span class="muted">idle</span></td>\s*<td><meter[^>]*value="0.0"`, body)
	assert.Regexp(t, `<span class="big">1</span><span>workers online</span>`, body)
}
