package store

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/sched"
)

// TestClaimRace pins the central invariant: never two live leases on one
// task, however many claims race.
func TestClaimRace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	workers := make([]string, 10)
	for i := range workers {
		workers[i] = f.worker(api.SandboxFull, f.pubID)
	}
	for range 10 {
		f.job(f.pubID, "c", api.WeightMedium, 5, 60) // 50 tasks
	}
	els := make([]Eligibility, len(workers))
	for i, w := range workers {
		els[i] = f.eligibility(w)
	}

	const claimers = 200
	var wg sync.WaitGroup
	leases := make([]*Lease, claimers)
	errs := make([]error, claimers)
	start := make(chan struct{})
	for i := range claimers {
		wg.Go(func() {
			<-start
			w := i % len(workers)
			leases[i], errs[i] = f.s.Claim(f.ctx, workers[w], els[w], ttl)
		})
	}
	close(start)
	wg.Wait()

	tasks := map[string]bool{}
	for i := range claimers {
		require.NoError(t, errs[i], "every claim succeeds or returns nil")
		if l := leases[i]; l != nil {
			key := fmt.Sprintf("%s/%d", l.JobID, l.Seq)
			assert.False(t, tasks[key], "task %s leased twice", key)
			tasks[key] = true
		}
	}
	assert.Len(t, tasks, 50)

	var dup int
	require.NoError(t, f.s.r.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM leases
		WHERE ended_at IS NULL GROUP BY job_id, seq HAVING COUNT(*) > 1)`).Scan(&dup))
	assert.Zero(t, dup)
	var pending int
	require.NoError(t, f.s.r.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM tasks WHERE state != 'leased'`).Scan(&pending))
	assert.Zero(t, pending)
}

func TestClaimEligibility(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	other := f.queue("other", 0)
	w := f.worker(api.SandboxFull, f.pubID)
	big, _, err := f.s.CreateJob(f.ctx, NewJob{QueueID: f.pubID, ClientID: "c",
		Params:    api.JobParams{Model: "large-v3-turbo"},
		Canonical: &Canonical{Samples: 100}, Ranges: []SampleRange{{0, 100}}})
	require.NoError(t, err)
	f.job(other, "c", api.WeightMedium, 1, 10) // not granted to w

	assert.Nil(t, f.claim(w), "model not offered, queue not granted")

	el := f.eligibility(w)
	el.Models = []string{"large-v3-turbo"}
	el.Kinds = []api.TaskKind{api.KindPrepare}
	l, err := f.s.Claim(f.ctx, w, el, ttl)
	require.NoError(t, err)
	assert.Nil(t, l, "transcribe not offered")

	el.Kinds = []api.TaskKind{api.KindTranscribe}
	l, err = f.s.Claim(f.ctx, w, el, ttl)
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, big.ID, l.JobID)
	assert.Equal(t, "public", l.QueueName)
	assert.Equal(t, 1, l.Attempt)
	assert.Equal(t, f.clk.Now().Add(ttl), l.ExpiresAt)
	assert.True(t, l.Live)

	_, err = f.s.Claim(f.ctx, "nobody", el, ttl)
	assert.ErrorIs(t, err, ErrNotFound)
}

// drain claims n tasks with w, completing each, and returns cost served per
// queue and per (queue, class).
func (f *fixture) drain(w string, n int) (map[int64]float64, map[string]float64) {
	byQueue, byClass := map[int64]float64{}, map[string]float64{}
	el := f.eligibility(w)
	for range n {
		l, err := f.s.Claim(f.ctx, w, el, ttl)
		require.NoError(f.t, err)
		require.NotNil(f.t, l)
		byQueue[l.QueueID] += l.Cost
		byClass[fmt.Sprintf("%d/%s", l.QueueID, l.Weight)] += l.Cost
		f.complete(l)
	}
	return byQueue, byClass
}

func TestClaimFairnessAcrossQueues(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.queue("a", 100)
	b := f.queue("b", 300)
	w := f.worker(api.SandboxFull, a, b)
	f.job(a, "c", api.WeightMedium, 100, 60)
	f.job(b, "c", api.WeightMedium, 100, 60)

	served, _ := f.drain(w, 80)
	assert.InDelta(t, 20*60, served[a], 60, "within one task cost")
	assert.InDelta(t, 60*60, served[b], 60)
}

func TestClaimClassSharesWithinQueue(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	// Low was submitted first: weighting, not FIFO, decides across classes.
	f.job(f.pubID, "c", api.WeightLow, 50, 60)
	f.job(f.pubID, "c", api.WeightMedium, 50, 60)
	f.job(f.pubID, "c", api.WeightHigh, 50, 60)

	_, served := f.drain(w, 70)
	q := f.pubID
	assert.InDelta(t, 40*60, served[fmt.Sprintf("%d/high", q)], 60)
	assert.InDelta(t, 20*60, served[fmt.Sprintf("%d/medium", q)], 60)
	assert.InDelta(t, 10*60, served[fmt.Sprintf("%d/low", q)], 60)
}

func TestClaimFIFOWithinClass(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	first := f.job(f.pubID, "c", api.WeightMedium, 2, 60)
	f.clk.Advance(time.Millisecond)
	second := f.job(f.pubID, "c", api.WeightMedium, 2, 60)
	var got []string
	for range 4 {
		l := f.claim(w)
		got = append(got, fmt.Sprintf("%s/%d", l.JobID, l.Seq))
	}
	assert.Equal(t, []string{first.ID + "/1", first.ID + "/2", second.ID + "/1", second.ID + "/2"}, got)
}

func TestReactivationThroughStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.queue("a", 100)
	b := f.queue("b", 100)
	w := f.worker(api.SandboxFull, a, b)
	f.job(a, "c", api.WeightMedium, 60, 60)

	// Queue b idles while a is served: it must not bank that time as credit.
	served, _ := f.drain(w, 40)
	require.Equal(t, 40*60.0, served[a])

	f.job(b, "c", api.WeightMedium, 60, 60)
	st, err := f.s.Stats(f.ctx, time.Hour)
	require.NoError(t, err)
	vt := map[string]float64{}
	for _, q := range st.Queues {
		vt[q.Name] = q.VTime
	}
	assert.Equal(t, vt["a"], vt["b"], "b starts at the backlogged minimum, not 0")

	served, _ = f.drain(w, 20)
	assert.InDelta(t, 10*60, served[a], 60, "a keeps its share after b wakes")
	assert.InDelta(t, 10*60, served[b], 60)
}

func TestRequeueAppliesReactivation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.queue("a", 100)
	b := f.queue("b", 100)
	w := f.worker(api.SandboxFull, a, b)
	f.job(a, "c", api.WeightMedium, 1, 60) // one task: a has no pending work once leased
	la := f.claim(w)
	f.job(b, "c", api.WeightMedium, 40, 60)
	f.drain(w, 30) // b runs alone; its vtime climbs past a's

	_, err := f.s.Release(f.ctx, la.ID, w) // a has pending work again
	require.NoError(t, err)
	vtimes, err := loadSched(f.ctx, f.s.r)
	require.NoError(t, err)
	assert.Equal(t, vtimes.Get(sched.QueueKey(b)), vtimes.Get(sched.QueueKey(a)),
		"requeue reactivates a at b's vtime")
}

func TestStats(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	require.NoError(t, f.s.TouchWorker(f.ctx, w, nil))
	_, err := f.s.RegisterWorker(f.ctx, "pending-token", api.WorkerFacts{})
	require.NoError(t, err)
	f.job(f.pubID, "c", api.WeightMedium, 3, 60)
	f.prepJob(f.pubID, "c")
	f.clk.Advance(time.Minute)
	f.claim(w)

	st, err := f.s.Stats(f.ctx, 10*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, map[api.WorkerState]int{api.WorkerActive: 1, api.WorkerPending: 1}, st.Workers)
	assert.ElementsMatch(t, []JobCount{{"public", api.JobRunning, 1}, {"public", api.JobPreparing, 1}}, st.Jobs)
	assert.Contains(t, st.Tasks, TaskCount{"public", api.KindTranscribe, api.TaskPending, 2})
	assert.Contains(t, st.Tasks, TaskCount{"public", api.KindTranscribe, api.TaskLeased, 1})
	require.Len(t, st.Queues, 1)
	q := st.Queues[0]
	assert.Equal(t, 3, q.PendingTasks)
	assert.Equal(t, 2*60+DefaultPrepareCost, q.BacklogCost)
	assert.Equal(t, time.Minute, q.OldestPending)
	assert.Equal(t, 1, q.EligibleWorkers)
	assert.Equal(t, 100, q.Weight)
	assert.Positive(t, q.VTime)
	assert.Positive(t, q.ClassVTimes[api.WeightMedium])
	assert.EqualValues(t, 100+1000, st.OriginalBytes)
	assert.EqualValues(t, 100, st.CanonicalBytes)

	f.clk.Advance(time.Hour)
	st, err = f.s.Stats(f.ctx, 10*time.Minute)
	require.NoError(t, err)
	assert.Zero(t, st.Queues[0].EligibleWorkers, "worker not seen recently")
}

func TestRetentionQueries(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	done := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.complete(f.claim(w))
	_, err := f.s.MarkJobDone(f.ctx, done.ID)
	require.NoError(t, err)
	failed := f.prepJob(f.pubID, "c")
	_, err = f.s.FailJob(f.ctx, failed.ID, "ffmpeg: invalid data")
	require.NoError(t, err)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10) // queued: never purged

	finished := f.clk.Now()
	f.clk.Advance(25 * time.Hour)

	none, err := f.s.MediaToPurge(f.ctx, finished.Add(-time.Millisecond))
	require.NoError(t, err)
	assert.Empty(t, none)

	ps, err := f.s.MediaToPurge(f.ctx, f.clk.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.ElementsMatch(t, []MediaPurge{
		{JobID: done.ID, Original: true, Canonical: true},
		{JobID: failed.ID, Original: true},
	}, ps)
	require.NoError(t, f.s.MarkOriginalPurged(f.ctx, done.ID))
	require.NoError(t, f.s.MarkCanonicalPurged(f.ctx, done.ID))
	require.NoError(t, f.s.MarkOriginalPurged(f.ctx, failed.ID))
	ps, err = f.s.MediaToPurge(f.ctx, f.clk.Now())
	require.NoError(t, err)
	assert.Empty(t, ps)
	got, err := f.s.Job(f.ctx, done.ID)
	require.NoError(t, err)
	assert.True(t, got.API(f.clk.Now()).Media.AudioPurged)

	ids, err := f.s.JobsToDelete(f.ctx, f.clk.Now().Add(-720*time.Hour))
	require.NoError(t, err)
	assert.Empty(t, ids)
	ids, err = f.s.JobsToDelete(f.ctx, f.clk.Now())
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{done.ID, failed.ID}, ids)

	n, err := f.s.DeleteEndedLeases(f.ctx, finished)
	require.NoError(t, err)
	assert.Zero(t, n)
	n, err = f.s.DeleteEndedLeases(f.ctx, f.clk.Now())
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
}
