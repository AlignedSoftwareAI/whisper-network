package server

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/blobs"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/store"
)

// manualTicker never ticks unless the test sends on it.
type manualTicker struct{ c chan time.Time }

func (m manualTicker) C() <-chan time.Time { return m.c }
func (m manualTicker) Stop()               {}

func TestReaperExpiresThenFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))

	l := h.claim(wid, wtok, "", nil)
	h.clk.Advance(h.srv.leaseTTL() - time.Second)
	h.srv.reapOnce(h.ctx)
	assert.Empty(t, h.logs.events(t, obs.EventLeaseExpired), "not yet expired")

	h.clk.Advance(2 * time.Second)
	h.srv.reapOnce(h.ctx)
	exp := h.logs.events(t, obs.EventLeaseExpired)
	require.Len(t, exp, 1)
	assert.Equal(t, l.ID, exp[0][obs.FieldLeaseID])
	assert.Equal(t, "warn", exp[0]["level"])
	assert.Equal(t, api.JobRunning, h.job(job.ID).State, "requeued, not failed")

	assert.Nil(t, h.claim(wid, wtok, "", nil), "never back to the worker it expired on")
	w2id, w2tok := h.activeWorker(api.WorkerFacts{}, "public")
	require.NotNil(t, h.claim(w2id, w2tok, "", nil))
	h.clk.Advance(h.srv.leaseTTL() + time.Second)
	h.srv.reapOnce(h.ctx)
	j := h.job(job.ID)
	assert.Equal(t, api.JobFailed, j.State, "expiries on two workers exhaust max_attempts")
	assert.Len(t, h.logs.events(t, obs.EventJobFailed), 1)

	var mb = &syncBuffer{}
	h.metrics.WritePrometheus(mb)
	assert.Contains(t, string(mb.Bytes()), `whisper_leases_ended_total{outcome="expired",kind="transcribe"} 2`)
	assert.Contains(t, string(mb.Bytes()), `whisper_jobs_finished_total{queue="public",outcome="failed"} 1`)
}

func TestReaperRetention(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	done := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	resp, b := h.putResult(l, wtok, result(l, "kept"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	// A job that failed during prepare keeps its original until retention.
	failed := h.submit(client, "public", api.JobParams{Model: "small.en"}, []byte("opaque video"))
	pl := h.claim(wid, wtok, "", nil)
	require.Equal(t, api.KindPrepare, pl.Kind)
	h.expect("POST", "/lease/"+pl.ID+"/failure", wtok, api.FailureReport{Reason: "ffmpeg: invalid data"},
		http.StatusNoContent, nil)
	require.Equal(t, api.JobFailed, h.job(failed.ID).State)
	pendingID, _ := h.worker(api.WorkerFacts{Hostname: "abandoned"})

	h.clk.Advance(h.srv.cfg.AudioRetention + time.Second)
	h.srv.reapOnce(h.ctx)
	j := h.job(done.ID)
	assert.True(t, j.Media.AudioPurged)
	_, err := os.Stat(blobPath(t, h, done.ID, blobs.Canonical))
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(blobPath(t, h, done.ID, blobs.CanonicalIndex))
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.True(t, h.job(failed.ID).Media.OriginalPurged)
	_, err = os.Stat(blobPath(t, h, failed.ID, blobs.Original))
	assert.ErrorIs(t, err, os.ErrNotExist)
	h.expect("GET", "/job/"+done.ID+"/transcript", client, nil, http.StatusOK, nil)

	h.clk.Advance(h.srv.cfg.JobRetention)
	h.srv.reapOnce(h.ctx)
	for _, id := range []string{done.ID, failed.ID} {
		resp, b := h.call("GET", "/job/"+id, h.admin, nil)
		h.expectProblem(resp, b, http.StatusNotFound, api.ProblemNotFound)
		_, err := os.Stat(blobPath(t, h, id, ""))
		assert.ErrorIs(t, err, os.ErrNotExist)
	}
	_, err = h.st.WorkerByID(h.ctx, pendingID)
	assert.ErrorIs(t, err, store.ErrNotFound, "pending worker unseen for the TTL is deleted")
	_, err = h.st.WorkerByID(h.ctx, wid)
	assert.NoError(t, err, "active workers stay")

	purged := h.logs.events(t, obs.EventJobPurged)
	var whats []any
	for _, p := range purged {
		whats = append(whats, p[obs.FieldWhat])
	}
	assert.Contains(t, whats, "canonical")
	assert.Contains(t, whats, "original")
	assert.NotEmpty(t, h.logs.events(t, obs.EventReaperRun))
}

// TestReaperAssemblesInterruptedJob: if the server stopped between the
// last result and assembly, the reaper finishes the job.
func TestReaperAssemblesInterruptedJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	_, err := h.st.CompleteTranscribe(h.ctx, l.ID, wid, store.TranscribeCompletion{
		Result:        []byte(`{"model":"small.en","segments":[{"start_ms":0,"end_ms":100,"text":"recovered"}]}`),
		EngineVersion: "whisper.cpp 1.9.5", ComputeMS: 10})
	require.NoError(t, err)
	assert.Equal(t, api.JobRunning, h.job(job.ID).State)

	h.srv.reapOnce(h.ctx)
	assert.Equal(t, api.JobDone, h.job(job.ID).State)
	_, tb := h.call("GET", "/job/"+job.ID+"/transcript", client, nil)
	assert.Contains(t, string(tb), "[00:00.00] recovered")
}

// TestRestartGrace: on start, live leases get one TTL of grace before the
// reaper may expire them (DESIGN.md §8.3).
func TestRestartGrace(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)

	h.clk.Advance(h.srv.leaseTTL() - 5*time.Second) // the server was down most of the TTL
	restarted, err := New(h.srv.cfg, Deps{Store: h.st, Blobs: h.blobs, Logger: h.srv.log, Now: h.clk.Now,
		FreeBytes: func(string) (int64, error) { return 1 << 40, nil }, LookupModel: lookupTestModel,
		NewTicker: func(time.Duration) Ticker { return manualTicker{make(chan time.Time)} }})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	require.NoError(t, restarted.Start(ctx))

	h.clk.Advance(30 * time.Second) // past the original expiry
	restarted.reapOnce(h.ctx)
	assert.Empty(t, h.logs.events(t, obs.EventLeaseExpired))
	h.expect("POST", "/lease/"+l.ID+"/heartbeat", wtok, nil, http.StatusOK, nil)
}

func TestSweepRemovesOrphans(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) { c.OrphanGrace = time.Minute })
	client := h.client(submitOn("public"))
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))

	require.NoError(t, h.blobs.WriteFile("orphan-job", blobs.Original, []byte("left by a crash")))
	stale, err := h.blobs.Create(job.ID, blobs.Transcript)
	require.NoError(t, err)
	require.NoError(t, stale.Close())
	fresh, err := h.blobs.Create(job.ID, blobs.Result)
	require.NoError(t, err)
	defer fresh.Abort()
	// The sweep reads the server's clock; file times are set relative to it.
	old := h.clk.Now().Add(-time.Hour)
	now := h.clk.Now()
	require.NoError(t, os.Chtimes(stale.Name(), old, old))
	require.NoError(t, os.Chtimes(fresh.Name(), now, now))
	require.NoError(t, os.Chtimes(blobPath(t, h, "orphan-job", blobs.Original), old, old))
	require.NoError(t, os.Chtimes(blobPath(t, h, "orphan-job", ""), old, old))

	h.srv.sweep(h.ctx)
	_, err = os.Stat(blobPath(t, h, "orphan-job", ""))
	assert.ErrorIs(t, err, os.ErrNotExist, "row-less directory removed")
	_, err = os.Stat(stale.Name())
	assert.ErrorIs(t, err, os.ErrNotExist, "old part file removed")
	_, err = os.Stat(fresh.Name())
	assert.NoError(t, err, "a write in progress is untouched")
	_, err = os.Stat(blobPath(t, h, job.ID, blobs.Canonical))
	assert.NoError(t, err, "live jobs are untouched")
}
