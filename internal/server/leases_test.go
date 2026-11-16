package server

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/store"
	"github.com/deweysasser/whisper-network/internal/testscale"
)

// TestFencing: a lease that expired cannot heartbeat or deliver, and its
// late result never replaces the one from the lease that superseded it.
func TestFencing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	slowID, slowTok := h.activeWorker(api.WorkerFacts{Hostname: "slow"}, "public")
	fastID, fastTok := h.activeWorker(api.WorkerFacts{Hostname: "fast"}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	require.Equal(t, 1, job.Tasks.Total)

	stale := h.claim(slowID, slowTok, "", nil)
	require.NotNil(t, stale)
	h.clk.Advance(h.srv.leaseTTL() + time.Second)
	h.srv.reapOnce(h.ctx)
	require.Len(t, h.logs.events(t, obs.EventLeaseExpired), 1)

	resp, b := h.call("POST", "/lease/"+stale.ID+"/heartbeat", slowTok, nil)
	h.expectProblem(resp, b, http.StatusGone, api.ProblemLeaseGone)
	resp, b = h.call("GET", stale.InputURL, slowTok, nil)
	h.expectProblem(resp, b, http.StatusGone, api.ProblemLeaseGone)

	fresh := h.claim(fastID, fastTok, "", nil)
	require.NotNil(t, fresh, "the expired task requeued")
	assert.Equal(t, stale.Seq, fresh.Seq)

	resp, b = h.putResult(stale, slowTok, result(stale, "stale words"))
	h.expectProblem(resp, b, http.StatusGone, api.ProblemLeaseGone)
	resp, b = h.putResult(fresh, fastTok, result(fresh, "fresh words"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	resp, b = h.putResult(stale, slowTok, result(stale, "stale words"))
	h.expectProblem(resp, b, http.StatusGone, api.ProblemLeaseGone)

	_, tb := h.call("GET", "/job/"+job.ID+"/transcript", client, nil)
	assert.Contains(t, string(tb), "fresh words")
	assert.NotContains(t, string(tb), "stale words")

	// Another worker's lease is not even visible.
	resp, b = h.call("POST", "/lease/"+fresh.ID+"/heartbeat", slowTok, nil)
	h.expectProblem(resp, b, http.StatusNotFound, api.ProblemNotFound)
	// A different result for a completed lease conflicts.
	resp, b = h.putResult(fresh, fastTok, result(fresh, "changed my mind"))
	h.expectProblem(resp, b, http.StatusConflict, api.ProblemConflict)
}

func TestJobDeletedMidLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	require.NotNil(t, l)

	h.expect("DELETE", "/job/"+job.ID, client, nil, http.StatusNoContent, nil)
	resp, b := h.call("POST", "/lease/"+l.ID+"/heartbeat", wtok, nil)
	h.expectProblem(resp, b, http.StatusGone, api.ProblemLeaseGone)
	resp, b = h.putResult(l, wtok, result(l, "too late"))
	h.expectProblem(resp, b, http.StatusGone, api.ProblemLeaseGone)
}

func TestInvalidTranscribeResultCountsAttempt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))

	l := h.claim(wid, wtok, "", nil)
	bad := result(l, "x")
	bad.Segments = []api.Segment{{StartMS: 0, EndMS: 60_000, Text: "beyond the task"}}
	resp, b := h.putResult(l, wtok, bad)
	h.expectProblem(resp, b, http.StatusUnprocessableEntity, api.ProblemResultInvalid)

	assert.Nil(t, h.claim(wid, wtok, "", nil), "not back to the worker that sent the bad result")
	w2id, w2tok := h.activeWorker(api.WorkerFacts{}, "public")
	l2 := h.claim(w2id, w2tok, "", nil)
	require.NotNil(t, l2, "requeued after one counted attempt")
	resp, b = h.call("PUT", "/lease/"+l2.ID+"/result", w2tok, "not an object")
	h.expectProblem(resp, b, http.StatusUnprocessableEntity, api.ProblemResultInvalid)
	assert.Equal(t, api.JobFailed, h.job(job.ID).State)
}

func TestFailureAndRelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))

	// Releases do not count: release many times, the task always returns.
	for i := 0; i < 3; i++ {
		l := h.claim(wid, wtok, "", nil)
		require.NotNil(t, l, "release %d", i)
		h.expect("DELETE", "/lease/"+l.ID, wtok, nil, http.StatusNoContent, nil)
		resp, b := h.call("DELETE", "/lease/"+l.ID, wtok, nil)
		h.expectProblem(resp, b, http.StatusGone, api.ProblemLeaseGone)
	}
	assert.Len(t, h.logs.events(t, obs.EventLeaseReleased), 3)

	l := h.claim(wid, wtok, "", nil)
	h.expect("POST", "/lease/"+l.ID+"/failure", wtok,
		api.FailureReport{Reason: "whisper-cli crashed", StderrTail: "segfault at 0x0", Retryable: true},
		http.StatusNoContent, nil)
	assert.Nil(t, h.claim(wid, wtok, "", nil), "never back to the worker that failed it")
	w2id, w2tok := h.activeWorker(api.WorkerFacts{}, "public")
	l = h.claim(w2id, w2tok, "", nil)
	require.NotNil(t, l, "retryable failure requeues")
	h.expect("POST", "/lease/"+l.ID+"/failure", w2tok, api.FailureReport{Reason: "out of memory", Retryable: true},
		http.StatusNoContent, nil)
	j := h.job(job.ID)
	assert.Equal(t, api.JobFailed, j.State)
	require.NotNil(t, j.Error)
	assert.Contains(t, *j.Error, "out of memory")
	failed := h.logs.events(t, obs.EventTaskFailed)
	require.Len(t, failed, 2)
	assert.Equal(t, "segfault at 0x0", failed[0][obs.FieldStderrTail])
}

func TestClaimWorkerStates(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id, tok := h.worker(api.WorkerFacts{})

	resp, b := h.call("POST", "/worker/"+id+"/leases", tok, nil)
	p := h.expectProblem(resp, b, http.StatusForbidden, api.ProblemWorkerPending)
	assert.Equal(t, "pending", p.State)
	var w api.Worker
	h.expect("GET", "/worker/"+id, tok, nil, http.StatusOK, &w)
	assert.Equal(t, api.WorkerPending, w.State)
	assert.Equal(t, 30, w.PollIntervalS)

	h.expect("POST", "/admin/workers/"+id+"/approve", h.admin, api.ApproveWorkerRequest{Queues: []string{"public"}},
		http.StatusNoContent, nil)
	h.expect("GET", "/worker/"+id, tok, nil, http.StatusOK, &w)
	assert.Equal(t, []string{"public"}, w.Queues)
	assert.Nil(t, h.claim(id, tok, "", nil), "active and idle")

	h.expect("POST", "/admin/workers/"+id+"/pause", h.admin, nil, http.StatusNoContent, nil)
	resp, b = h.call("POST", "/worker/"+id+"/leases", tok, nil)
	h.expectProblem(resp, b, http.StatusConflict, api.ProblemWorkerPaused)
	h.expect("POST", "/admin/workers/"+id+"/resume", h.admin, nil, http.StatusNoContent, nil)
	assert.Nil(t, h.claim(id, tok, "", nil))

	// Another worker's id is invisible.
	other, otherTok := h.worker(api.WorkerFacts{})
	resp, b = h.call("POST", "/worker/"+id+"/leases", otherTok, nil)
	h.expectProblem(resp, b, http.StatusNotFound, api.ProblemNotFound)
	resp, b = h.call("GET", "/worker/"+id, otherTok, nil)
	h.expectProblem(resp, b, http.StatusNotFound, api.ProblemNotFound)
	_ = other

	// Rejection is terminal: every call with the token says so with 410.
	client := h.client(submitOn("public"))
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	l := h.claim(id, tok, "", nil)
	require.NotNil(t, l)
	h.expect("POST", "/admin/workers/"+id+"/reject", h.admin, nil, http.StatusNoContent, nil)
	for _, path := range []string{"/worker/" + id + "/leases", "/lease/" + l.ID + "/heartbeat"} {
		resp, b = h.call("POST", path, tok, nil)
		p = h.expectProblem(resp, b, http.StatusGone, api.ProblemWorkerRejected)
		assert.Equal(t, "rejected", p.State)
	}
	resp, b = h.call("POST", "/admin/workers/"+id+"/resume", h.admin, nil)
	h.expectProblem(resp, b, http.StatusConflict, api.ProblemConflict)
	for _, ev := range []obs.Event{obs.EventWorkerPaused, obs.EventWorkerResumed, obs.EventWorkerRejected} {
		assert.Len(t, h.logs.events(t, ev), 1, ev)
	}
}

func TestPatchWorker(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id, tok := h.worker(api.WorkerFacts{})
	var w api.Worker
	h.expect("PATCH", "/worker/"+id, tok, api.WorkerFacts{Hostname: "renamed", SandboxLevel: api.SandboxNone,
		EngineVersion: "whisper.cpp 1.9.6"}, http.StatusOK, &w)
	assert.Equal(t, "renamed", w.Facts.Hostname)
	assert.Equal(t, "renamed", w.Facts.Description, "description defaults to the hostname")
	resp, b := h.call("PATCH", "/worker/"+id, tok, api.WorkerFacts{Hostname: "x", SandboxLevel: "superb"})
	h.expectProblem(resp, b, http.StatusBadRequest, api.ProblemBadRequest)
	resp, b = h.call("PATCH", "/worker/"+id, h.admin, api.WorkerFacts{Hostname: "x"})
	h.expectProblem(resp, b, http.StatusForbidden, api.ProblemForbidden)
}

// TestLongPoll: a waiting claim returns promptly when work arrives, and
// with 204 when its wait runs out.
func TestLongPoll(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")

	start := time.Now()
	assert.Nil(t, h.claim(wid, wtok, "300ms", nil))
	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "waited for the full wait")

	// A claim that ran out its wait would return no lease, so a lease is
	// proof of the wake-up; the wait is long and scaled so that a loaded
	// runner cannot mistake a slow wake for a timeout.
	wait := testscale.D(8 * time.Second)
	got := make(chan *api.Lease, 1)
	go func() { got <- h.claim(wid, wtok, wait.String(), nil) }()
	time.Sleep(200 * time.Millisecond) // let the claim start waiting
	submitted := time.Now()
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	select {
	case l := <-got:
		require.NotNil(t, l, "woken by the new job, not the timeout")
		assert.Less(t, time.Since(submitted), wait, "woken before the wait ran out")
	case <-time.After(wait + testscale.D(2*time.Second)):
		t.Fatal("claim was not woken by the new job")
	}

	resp, b := h.call("POST", "/worker/"+wid+"/leases?wait=soon", wtok, nil)
	h.expectProblem(resp, b, http.StatusBadRequest, api.ProblemBadRequest)
}

// TestLongPollWakesOnRequeue: a release wakes a waiting claim.
func TestLongPollWakesOnRequeue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	aID, aTok := h.activeWorker(api.WorkerFacts{}, "public")
	bID, bTok := h.activeWorker(api.WorkerFacts{}, "public")
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	l := h.claim(aID, aTok, "", nil)
	require.NotNil(t, l)

	wait := testscale.D(8 * time.Second)
	got := make(chan *api.Lease, 1)
	go func() { got <- h.claim(bID, bTok, wait.String(), nil) }()
	time.Sleep(200 * time.Millisecond)
	h.expect("DELETE", "/lease/"+l.ID, aTok, nil, http.StatusNoContent, nil)
	select {
	case l2 := <-got:
		require.NotNil(t, l2, "woken by the release, not the timeout")
		assert.Equal(t, l.Seq, l2.Seq)
	case <-time.After(wait + testscale.D(2*time.Second)):
		t.Fatal("claim was not woken by the release")
	}
}

// TestGracefulShutdown: Serve returns promptly on cancellation, and a
// waiting long-poll is answered with 204 rather than cut off.
func TestGracefulShutdown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	srv, err := New(h.srv.cfg, Deps{Store: h.st, Blobs: h.blobs, Logger: h.srv.log, Now: h.clk.Now,
		FreeBytes: func(string) (int64, error) { return 1 << 40, nil }, LookupModel: lookupTestModel})
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln, nil) }()

	url := "http://" + ln.Addr().String()
	status := make(chan int, 1)
	go func() {
		r, _ := http.NewRequest("POST", url+"/worker/"+wid+"/leases?wait=30s", nil)
		r.Header.Set("Authorization", "Bearer "+wtok)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			status <- -1
			return
		}
		resp.Body.Close()
		status <- resp.StatusCode
	}()
	require.Eventually(t, func() bool {
		resp, err := http.Get(url + "/healthz")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond) // the claim is waiting

	start := time.Now()
	cancel()
	select {
	case code := <-status:
		assert.Equal(t, http.StatusNoContent, code)
	case <-time.After(5 * time.Second):
		t.Fatal("long-poll did not return on shutdown")
	}
	select {
	case err := <-served:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	assert.Less(t, time.Since(start), 3*time.Second)
	_, err = http.Get(url + "/healthz")
	assert.Error(t, err, "listener closed")
}

// TestWeightedFairness: two backlogged queues with weights 100 and 300
// served by one worker split claims about 1:3 (DESIGN.md §8.2).
func TestWeightedFairness(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) {
		c.TaskTarget, c.SplitWindow = time.Second, 300*time.Millisecond
	})
	for name, w := range map[string]int{"light": 100, "heavy": 300} {
		h.expect("POST", "/admin/queues", h.admin, api.Queue{Name: name, Weight: w}, http.StatusCreated, nil)
	}
	client := h.client(submitOn("light"), submitOn("heavy"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "light", "heavy")
	audio := canonicalWAV(speech(45, media.SampleRate)) // ~45 tasks of ~1 s
	light := h.submit(client, "light", api.JobParams{Model: "small.en"}, audio)
	heavy := h.submit(client, "heavy", api.JobParams{Model: "small.en"}, audio)
	require.Greater(t, light.Tasks.Total, 35)

	served := map[string]int{}
	for i := 0; i < 40; i++ {
		l := h.claim(wid, wtok, "", nil)
		require.NotNil(t, l)
		served[l.Job]++
	}
	assert.InDelta(t, 10, served[light.ID], 2, "light share, got %v", served)
	assert.InDelta(t, 30, served[heavy.ID], 2, "heavy share, got %v", served)
}

func TestHeartbeatRecordsGauges(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	h.clk.Advance(time.Minute)
	var hb api.HeartbeatResponse
	h.expect("POST", "/lease/"+l.ID+"/heartbeat", wtok, api.HeartbeatRequest{Gauges: &api.Gauges{SlotsBusy: 2,
		ChildRSSBytes: 1 << 30}}, http.StatusOK, &hb)
	assert.Equal(t, h.clk.Now().Add(h.srv.leaseTTL()), hb.ExpiresAt)

	var ws []api.WorkerSummary
	h.expect("GET", "/admin/workers", h.admin, nil, http.StatusOK, &ws)
	require.Len(t, ws, 1)
	require.NotNil(t, ws[0].Gauges)
	assert.Equal(t, 2, ws[0].Gauges.SlotsBusy)
	assert.Equal(t, h.clk.Now(), *ws[0].LastSeenAt)
	assert.Empty(t, h.logs.events(t, obs.EventAuthDenied))
	// Heartbeat access lines log at debug.
	for _, e := range h.logs.events(t, obs.EventHTTPRequest) {
		if e[obs.FieldRoute] == "/lease/{id}/heartbeat" {
			assert.Equal(t, "debug", e["level"])
		}
	}
}

// TestServeCancelledDuringStartup: a shutdown signal that arrives while the
// server is still starting (here: before Serve is even called) is a clean
// exit, not an error. ServerCmd.Run surfaced "extend live leases: context
// canceled" when SIGTERM raced startup.
func TestServeCancelledDuringStartup(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	srv, err := New(h.srv.cfg, Deps{Store: h.st, Blobs: h.blobs, Logger: h.srv.log, Now: h.clk.Now,
		FreeBytes: func(string) (int64, error) { return 1 << 40, nil }, LookupModel: lookupTestModel})
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.NoError(t, srv.Serve(ctx, ln, nil))
}

// TestClaimReleasedWhenWorkerDisconnects: a worker that disconnects while its
// claim commits never sees the lease, so the server releases it at once: the
// task is pending again with no attempt consumed, and another claim gets it.
func TestClaimReleasedWhenWorkerDisconnects(t *testing.T) {
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))

	ctx, cancel := context.WithCancel(h.ctx)
	// The worker goes away right after the commit. net/http notices a
	// disconnect asynchronously, so wait until the server has seen it.
	h.srv.afterClaim = func(reqCtx context.Context) {
		cancel()
		select {
		case <-reqCtx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", h.ts.URL+"/worker/"+wid+"/leases", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+wtok)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	h.srv.afterClaim = nil

	require.Eventually(t, func() bool {
		j := h.job(job.ID)
		return j.Tasks.Pending == 1 && j.Tasks.Leased == 0
	}, 5*time.Second, 20*time.Millisecond, "orphaned lease released and the task requeued")
	assert.Contains(t, string(h.logs.Bytes()), "worker disconnected before receiving the lease")

	l := h.claim(wid, wtok, "", nil)
	require.NotNil(t, l, "the task is claimable again at once")
	assert.Equal(t, job.ID, l.Job)
	assert.Equal(t, 1, h.job(job.ID).Tasks.Leased)
}
