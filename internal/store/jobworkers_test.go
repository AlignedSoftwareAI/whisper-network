package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

// TestLiveLeasesAndCompletedWorkers: only leases that are neither ended nor
// past their expiry are live; a job can be on several workers at once; a
// worker's live leases span jobs; completed tasks name their workers.
func TestLiveLeasesAndCompletedWorkers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w1 := f.worker(api.SandboxFull, f.pubID)
	w2 := f.worker(api.SandboxFull, f.pubID)
	require.NoError(t, f.s.UpdateWorkerFacts(f.ctx, w1, api.WorkerFacts{Hostname: "box-1", Description: "first"}))
	require.NoError(t, f.s.UpdateWorkerFacts(f.ctx, w2, api.WorkerFacts{Hostname: "box-2"}))
	j := f.job(f.pubID, "c1", api.WeightMedium, 4, 10)
	k := f.job(f.pubID, "c2", api.WeightMedium, 1, 10)
	start := f.clk.Now()

	l1 := f.claim(w1) // j/1: heartbeated, stays live
	l2 := f.claim(w2) // j/2: never heartbeated, expires (not yet reaped)
	l3 := f.claim(w1) // j/3: completed
	l4 := f.claim(w2) // j/4: released
	l5 := f.claim(w2) // k/1: heartbeated, stays live
	require.Equal(t, []string{j.ID, j.ID, j.ID, j.ID, k.ID}, []string{l1.JobID, l2.JobID, l3.JobID, l4.JobID, l5.JobID})

	f.complete(l3)
	_, err := f.s.Release(f.ctx, l4.ID, w2)
	require.NoError(t, err)

	// Before any expiry: j has two live leases on two workers.
	live, err := f.s.LiveLeasesForJobs(f.ctx, []string{j.ID})
	require.NoError(t, err)
	require.Len(t, live, 2)
	assert.Equal(t, LiveLease{ID: l1.ID, JobID: j.ID, Seq: 1, Kind: api.KindTranscribe,
		Worker:  WorkerRef{ID: w1, Hostname: "box-1", Description: "first"},
		Attempt: 1, QueueName: PublicQueue, ClientID: "c1", CreatedAt: start, ExpiresAt: start.Add(ttl)}, live[0])
	assert.Equal(t, w2, live[1].Worker.ID)
	assert.Equal(t, 2, live[1].Seq)

	f.clk.Advance(30 * time.Second)
	hb := f.clk.Now()
	for _, l := range []*Lease{l1, l5} {
		_, err := f.s.Heartbeat(f.ctx, l.ID, l.WorkerID, ttl, LeaseAgeLimit{})
		require.NoError(t, err)
	}
	f.clk.Advance(ttl - 29*time.Second) // l2 is past its expiry, the others are not

	live, err = f.s.LiveLeasesForJobs(f.ctx, []string{j.ID, k.ID, "job-unknown"})
	require.NoError(t, err)
	require.Len(t, live, 2, "expired, completed and released leases are not live")
	assert.Equal(t, []string{l1.ID, l5.ID}, []string{live[0].ID, live[1].ID})
	require.NotNil(t, live[0].HeartbeatAt)
	assert.Equal(t, hb, *live[0].HeartbeatAt)
	assert.Equal(t, hb.Add(ttl), live[0].ExpiresAt)
	assert.Equal(t, start, live[0].CreatedAt, "a heartbeat does not move the lease's age")
	assert.Equal(t, "c2", live[1].ClientID)

	byWorker, err := f.s.LiveLeasesForWorkers(f.ctx, []string{w1, w2})
	require.NoError(t, err)
	require.Len(t, byWorker, 2)
	assert.Equal(t, []string{w1, w2}, []string{byWorker[0].Worker.ID, byWorker[1].Worker.ID})
	assert.Equal(t, []string{l1.ID, l5.ID}, []string{byWorker[0].ID, byWorker[1].ID})

	none, err := f.s.LiveLeasesForWorkers(f.ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, none)

	done, err := f.s.CompletedTaskWorkers(f.ctx, []string{j.ID, k.ID})
	require.NoError(t, err)
	assert.Equal(t, []JobWorker{{JobID: j.ID, Worker: WorkerRef{ID: w1, Hostname: "box-1", Description: "first"}}}, done)

	// The reaper ends l2; a second attempt by w1 is attempt 2.
	_, err = f.s.ExpireLeases(f.ctx)
	require.NoError(t, err)
	l6 := f.claim(w1)
	require.Equal(t, 2, l6.Seq)
	live, err = f.s.LiveLeasesForJobs(f.ctx, []string{j.ID})
	require.NoError(t, err)
	require.Len(t, live, 2)
	assert.Equal(t, 2, live[1].Attempt)
	assert.Nil(t, live[1].HeartbeatAt)
}

// TestCompletedTaskWorkersDistinct: a worker that finished several tasks
// of a job is listed once, in order of its first task.
func TestCompletedTaskWorkersDistinct(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w1 := f.worker(api.SandboxFull, f.pubID)
	w2 := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 3, 10)
	a, b, c := f.claim(w2), f.claim(w1), f.claim(w2)
	for _, l := range []*Lease{a, b, c} {
		f.complete(l)
	}
	got, err := f.s.CompletedTaskWorkers(f.ctx, []string{j.ID})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []string{w2, w1}, []string{got[0].Worker.ID, got[1].Worker.ID})
}

// TestLiveLeaseQueriesUseIndexes pins the query plans to the partial live
// lease indexes, so listing a page of jobs or the workers page never scans
// the (retention-sized) leases table.
func TestLiveLeaseQueriesUseIndexes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for k, index := range map[liveLeaseKey]string{liveByJob: "leases_one_live", liveByWorker: "leases_worker_live"} {
		rows, err := f.s.r.QueryContext(f.ctx, `EXPLAIN QUERY PLAN `+k.query(2), int64(0), "a", "b")
		require.NoError(t, err)
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			plan = append(plan, detail)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		joined := strings.Join(plan, "\n")
		assert.Contains(t, joined, "SEARCH l USING INDEX "+index, joined)
	}
}
