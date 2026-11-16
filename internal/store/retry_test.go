package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/sched"
)

// failClaim claims with w (which must get a task) and fails it retryably.
func (f *fixture) failClaim(w string) *Lease {
	f.t.Helper()
	l := f.claim(w)
	require.NotNil(f.t, l)
	end, err := f.s.Fail(f.ctx, l.ID, w, "broken", true)
	require.NoError(f.t, err)
	require.True(f.t, end.Requeued)
	return l
}

func (f *fixture) vtimes() *sched.State {
	f.t.Helper()
	st, err := loadSched(f.ctx, f.s.r)
	require.NoError(f.t, err)
	return st
}

// TestRetryElsewhereAfterFailure: the worker that failed a task never gets
// it back, however long it waits; another worker does.
func TestRetryElsewhereAfterFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.worker(api.SandboxFull, f.pubID)
	b := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.failClaim(a)

	assert.Nil(t, f.claim(a), "a failed this task")
	f.clk.Advance(30 * 24 * time.Hour)
	assert.Nil(t, f.claim(a), "not even long after")
	why, err := f.s.RetryExclusion(f.ctx, j.ID, 1, a)
	require.NoError(t, err)
	assert.Equal(t, "worker "+a+" failed this task", why)
	why, err = f.s.RetryExclusion(f.ctx, j.ID, 1, b)
	require.NoError(t, err)
	assert.Empty(t, why)

	l := f.claim(b)
	require.NotNil(t, l)
	assert.Equal(t, j.ID, l.JobID)
	assert.Equal(t, 2, l.Attempt)
}

// TestRetryElsewhereExpiryCounts: an expired lease excludes its worker like
// a failure does.
func TestRetryElsewhereExpiryCounts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.worker(api.SandboxFull, f.pubID)
	b := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	require.NotNil(t, f.claim(a))
	f.clk.Advance(ttl)
	ends, err := f.s.ExpireLeases(f.ctx)
	require.NoError(t, err)
	require.Len(t, ends, 1)
	require.True(t, ends[0].Requeued)

	assert.Nil(t, f.claim(a))
	why, err := f.s.RetryExclusion(f.ctx, j.ID, 1, a)
	require.NoError(t, err)
	assert.Equal(t, "a lease on this task expired on worker "+a, why)
	require.NotNil(t, f.claim(b))
}

// TestRetryElsewhereReleaseDoesNotExclude: a voluntary release is not a
// failure; the same worker may take the task straight back.
func TestRetryElsewhereReleaseDoesNotExclude(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.worker(api.SandboxFull, f.pubID)
	f.worker(api.SandboxFull, f.pubID) // an idle peer changes nothing
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	l := f.claim(a)
	_, err := f.s.Release(f.ctx, l.ID, a)
	require.NoError(t, err)

	again := f.claim(a)
	require.NotNil(t, again)
	assert.Equal(t, l.JobID, again.JobID)
	assert.Equal(t, 1, again.Attempt)
}

// TestAttemptsCountDistinctWorkers: with MaxAttempts 3 the task survives
// failures on two workers, fails on the third, and the job error names all
// three once each.
func TestAttemptsCountDistinctWorkers(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(c *Config) { c.MaxAttempts = 3 })
	ws := []string{f.worker(api.SandboxFull, f.pubID), f.worker(api.SandboxFull, f.pubID),
		f.worker(api.SandboxFull, f.pubID)}
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	for i, w := range ws[:2] {
		l := f.failClaim(w)
		assert.Equal(t, i+1, l.Attempt)
	}
	l := f.claim(ws[2])
	require.NotNil(t, l)
	assert.Equal(t, 3, l.Attempt)
	end, err := f.s.Fail(f.ctx, l.ID, ws[2], "out of memory", true)
	require.NoError(t, err)
	assert.True(t, end.JobFailed)
	assert.Equal(t, 3, end.Attempt)
	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, "transcribe task 1 failed after 3 attempt(s) (on workers "+strings.Join(ws, ", ")+
		"): out of memory; 0 of 1 tasks done, 0 cancelled", got.Error)
}

// TestExpiryAndFailureOnOneWorkerCountOnce: a worker that both let a lease
// expire and failed the task (possible with leases from before strict
// exclusion) is one attempt, and the job error names it once.
func TestExpiryAndFailureOnOneWorkerCountOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.worker(api.SandboxFull, f.pubID)
	b := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	l := f.claim(a)
	require.NotNil(t, l)
	f.clk.Advance(ttl)
	ends, err := f.s.ExpireLeases(f.ctx)
	require.NoError(t, err)
	require.Len(t, ends, 1)
	assert.Equal(t, 1, ends[0].Attempt)

	// Record a second, failed lease of a on the task, as an older server
	// could have; the strict claim no longer hands it out.
	_, err = f.s.w.ExecContext(f.ctx, `INSERT INTO leases (id, job_id, seq, worker_id, created_at, expires_at,
		ended_at, outcome) VALUES ('old', ?, 1, ?, ?, ?, ?, 'failed')`, j.ID, a,
		toMS(f.clk.Now()), toMS(f.clk.Now()), toMS(f.clk.Now()))
	require.NoError(t, err)

	l = f.claim(b)
	require.NotNil(t, l, "a counts once, so b may still try")
	assert.Equal(t, 2, l.Attempt)
	end, err := f.s.Fail(f.ctx, l.ID, b, "broken", true)
	require.NoError(t, err)
	require.True(t, end.JobFailed)
	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, "transcribe task 1 failed after 2 attempt(s) (on workers "+a+", "+b+"): broken; 0 of 1 tasks done, 0 cancelled", got.Error)
}

// TestNonRetryableFailsAtOnce: a failure the worker attributes to the input
// fails the job straight away, though other workers could retry.
func TestNonRetryableFailsAtOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(c *Config) { c.MaxAttempts = 5 })
	a := f.worker(api.SandboxFull, f.pubID)
	f.worker(api.SandboxFull, f.pubID)
	j := f.prepJob(f.pubID, "c")
	l := f.claim(a)
	require.NotNil(t, l)
	end, err := f.s.Fail(f.ctx, l.ID, a, "invalid data found when processing input", false)
	require.NoError(t, err)
	assert.True(t, end.JobFailed)
	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobFailed, got.State)
	assert.Equal(t, "prepare task 0 failed after 1 attempt(s) (on worker "+a+
		"): invalid data found when processing input", got.Error)
}

// TestFailStranded: a task that failed on its only eligible worker waits
// for another one, and fails its job once it has waited the limit, naming
// the worker. Until then nothing happens to it.
func TestFailStranded(t *testing.T) {
	t.Parallel()
	const limit = 24 * time.Hour
	f := newFixture(t)
	a := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 2, 10)
	other := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	l := f.claim(a)
	require.NotNil(t, l)
	_, err := f.s.Fail(f.ctx, l.ID, a, "model file corrupt", true)
	require.NoError(t, err)
	sibling := f.claim(a)
	require.NotNil(t, sibling, "a gets the job's other task")

	f.clk.Advance(limit - time.Millisecond)
	st, err := f.s.FailStranded(f.ctx, limit)
	require.NoError(t, err)
	assert.Empty(t, st, "not waited long enough")

	f.clk.Advance(time.Millisecond)
	st, err = f.s.FailStranded(f.ctx, limit)
	require.NoError(t, err)
	require.Len(t, st, 1)
	assert.Equal(t, j.ID, st[0].JobID)
	assert.Equal(t, []string{a}, st[0].Workers)
	want := "transcribe task 1 failed after 1 attempt(s) (on worker " + a +
		"); no other eligible worker took it within 24h: model file corrupt"
	assert.Equal(t, want, st[0].Reason)
	assert.False(t, st[0].JobFailed, "the sibling's lease is live")
	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobRunning, got.State, "failing: the leased sibling runs to completion")
	assert.Equal(t, want, got.Error)

	// The sibling's lease (long past its expiry here) is reaped: its task
	// is cancelled rather than requeued, and the job is failed.
	ends, err := f.s.ExpireLeases(f.ctx)
	require.NoError(t, err)
	require.Len(t, ends, 1)
	assert.Equal(t, sibling.ID, ends[0].LeaseID)
	assert.True(t, ends[0].JobFailed)
	assert.False(t, ends[0].Requeued)
	got, err = f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobFailed, got.State)
	assert.Equal(t, want+"; 0 of 2 tasks done, 1 cancelled", got.Error)
	assert.Equal(t, api.TaskCounts{Total: 2, Failed: 1, Cancelled: 1}, got.Tasks)

	got, err = f.s.Job(f.ctx, other.ID)
	require.NoError(t, err)
	assert.NotEqual(t, api.JobFailed, got.State, "a job that never failed waits as long as it needs")
	st, err = f.s.FailStranded(f.ctx, limit)
	require.NoError(t, err)
	assert.Empty(t, st)
}

// TestFailStrandedWaitRestartsOnRelease: a worker that takes the task and
// releases it shows the task can still be taken; the wait counts from the
// release.
func TestFailStrandedWaitRestartsOnRelease(t *testing.T) {
	t.Parallel()
	const limit = time.Hour
	f := newFixture(t)
	a := f.worker(api.SandboxFull, f.pubID)
	b := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.failClaim(a)
	f.clk.Advance(limit / 2)
	l := f.claim(b)
	require.NotNil(t, l)
	_, err := f.s.Release(f.ctx, l.ID, b)
	require.NoError(t, err)

	f.clk.Advance(limit / 2)
	st, err := f.s.FailStranded(f.ctx, limit)
	require.NoError(t, err)
	assert.Empty(t, st, "b took it half an hour ago")
	f.clk.Advance(limit / 2)
	st, err = f.s.FailStranded(f.ctx, limit)
	require.NoError(t, err)
	require.Len(t, st, 1)
	assert.Equal(t, j.ID, st[0].JobID)
	assert.Equal(t, []string{a}, st[0].Workers, "a release is not a failure")
}

// TestEndedLeasesOfLiveTasksKept: lease retention does not delete the
// failures of a task that is still pending, which would let its worker
// have it back and refund its attempts.
func TestEndedLeasesOfLiveTasksKept(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.worker(api.SandboxFull, f.pubID)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	done := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.failClaim(a)
	l := f.claim(a)
	require.NotNil(t, l)
	require.Equal(t, done.ID, l.JobID)
	f.complete(l)

	f.clk.Advance(time.Hour)
	n, err := f.s.DeleteEndedLeases(f.ctx, f.clk.Now())
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "only the finished task's lease")
	assert.Nil(t, f.claim(a), "still excluded")
}

// TestRetryElsewhereFairness: a (queue, class) whose only pending task is
// excluded for the worker is not its candidate, so the worker gets other
// work instead of nothing, and the excluded task's queue and class are not
// charged for it.
func TestRetryElsewhereFairness(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	other := f.queue("other", 100)
	a := f.worker(api.SandboxFull, f.pubID, other)
	b := f.worker(api.SandboxFull, f.pubID, other)

	// The failed task's queue and class come first in the schedule (other
	// reactivates at public's vtime and loses the tie on queue id; high
	// beats low): without the exclusion in the candidate query, (public,
	// high) would be picked and the claim would find nothing to lease in it.
	failed := f.job(f.pubID, "c", api.WeightHigh, 1, 10)
	f.failClaim(a)
	f.job(other, "c", api.WeightMedium, 1, 10)
	low := f.job(f.pubID, "c", api.WeightLow, 1, 10)

	before := f.vtimes()
	require.Less(t, f.pubID, other)
	require.LessOrEqual(t, before.Get(sched.QueueKey(f.pubID)), before.Get(sched.QueueKey(other)))
	require.LessOrEqual(t, before.Get(sched.Key{Queue: f.pubID, Class: api.WeightHigh}),
		before.Get(sched.Key{Queue: f.pubID, Class: api.WeightLow}))
	l := f.claim(a)
	require.NotNil(t, l, "a still gets work")
	assert.Equal(t, low.ID, l.JobID, "the next pending task of the same queue, in another class")
	after := f.vtimes()
	high := sched.Key{Queue: f.pubID, Class: api.WeightHigh}
	assert.Equal(t, before.Get(high), after.Get(high), "the excluded task's class is not charged")
	assert.Equal(t, before.Get(sched.QueueKey(f.pubID))+l.Cost/100, after.Get(sched.QueueKey(f.pubID)),
		"the queue is charged for the task a took, once")

	l = f.claim(a)
	require.NotNil(t, l)
	assert.Equal(t, other, l.QueueID, "another queue's work is still claimable")
	assert.Nil(t, f.claim(a), "only the excluded task is left")

	l = f.claim(b)
	require.NotNil(t, l)
	assert.Equal(t, failed.ID, l.JobID)
	assert.Greater(t, f.vtimes().Get(high), after.Get(high), "the retry is charged to whoever takes it")
}

// TestFailedJobNamesWorkers: the job error lists the worker of each failed
// attempt.
func TestFailedJobNamesWorkers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.worker(api.SandboxFull, f.pubID)
	b := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.failClaim(a)
	l := f.claim(b)
	require.NotNil(t, l)
	_, err := f.s.Fail(f.ctx, l.ID, b, "out of memory", true)
	require.NoError(t, err)
	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, "transcribe task 1 failed after 2 attempt(s) (on workers "+a+", "+b+"): out of memory; 0 of 1 tasks done, 0 cancelled", got.Error)
}
