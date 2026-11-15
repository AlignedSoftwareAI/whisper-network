package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/sched"
)

const grace = 10 * time.Minute

func withGrace(g time.Duration) func(*Config) {
	return func(c *Config) { c.RetryElsewhereGrace = g }
}

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

// TestRetryElsewhereAfterFailure: the worker that failed a task does not get
// it back while another worker could take it; the other worker does.
func TestRetryElsewhereAfterFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t, withGrace(grace))
	a := f.worker(api.SandboxFull, f.pubID)
	b := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.failClaim(a)

	assert.Nil(t, f.claim(a), "a failed this task")
	f.clk.Advance(grace - time.Millisecond)
	assert.Nil(t, f.claim(a), "still within the grace")
	l := f.claim(b)
	require.NotNil(t, l)
	assert.Equal(t, j.ID, l.JobID)
	assert.Equal(t, 2, l.Attempt)
	assert.False(t, l.SameWorkerRetry)
}

// TestRetryElsewhereExpiryCounts: an expired lease excludes its worker like
// a failure does.
func TestRetryElsewhereExpiryCounts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, withGrace(grace))
	a := f.worker(api.SandboxFull, f.pubID)
	b := f.worker(api.SandboxFull, f.pubID)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	require.NotNil(t, f.claim(a))
	f.clk.Advance(ttl)
	ends, err := f.s.ExpireLeases(f.ctx)
	require.NoError(t, err)
	require.Len(t, ends, 1)
	require.True(t, ends[0].Requeued)

	assert.Nil(t, f.claim(a))
	l := f.claim(b)
	require.NotNil(t, l)
	assert.False(t, l.SameWorkerRetry)
}

// TestRetryElsewhereReleaseDoesNotExclude: a voluntary release is not a
// failure; the same worker may take the task straight back.
func TestRetryElsewhereReleaseDoesNotExclude(t *testing.T) {
	t.Parallel()
	f := newFixture(t, withGrace(grace))
	a := f.worker(api.SandboxFull, f.pubID)
	f.worker(api.SandboxFull, f.pubID) // an idle peer changes nothing
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	l := f.claim(a)
	_, err := f.s.Release(f.ctx, l.ID, a)
	require.NoError(t, err)

	again := f.claim(a)
	require.NotNil(t, again)
	assert.Equal(t, l.JobID, again.JobID)
	assert.False(t, again.SameWorkerRetry)
}

// TestRetryElsewhereOnlyWorkerAfterGrace: when no other worker takes the
// retry, the worker that failed the task gets it once the grace has passed,
// and the lease says it is a same-worker retry.
func TestRetryElsewhereOnlyWorkerAfterGrace(t *testing.T) {
	t.Parallel()
	f := newFixture(t, withGrace(grace))
	a := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.failClaim(a)

	f.clk.Advance(grace - time.Millisecond)
	assert.Nil(t, f.claim(a))
	f.clk.Advance(time.Millisecond)
	l := f.claim(a)
	require.NotNil(t, l, "the grace has passed")
	assert.Equal(t, j.ID, l.JobID)
	assert.Equal(t, 2, l.Attempt)
	assert.True(t, l.SameWorkerRetry)

	end, err := f.s.Fail(f.ctx, l.ID, a, "broken again", true)
	require.NoError(t, err)
	require.True(t, end.JobFailed)
	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, "transcribe task 1 failed after 2 attempt(s) (on workers "+a+", "+a+"): broken again", got.Error)
}

// TestRetryElsewhereGraceZeroDisables: with no grace the failing worker may
// retry at once.
func TestRetryElsewhereGraceZeroDisables(t *testing.T) {
	t.Parallel()
	f := newFixture(t, withGrace(0))
	a := f.worker(api.SandboxFull, f.pubID)
	f.worker(api.SandboxFull, f.pubID)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.failClaim(a)

	l := f.claim(a)
	require.NotNil(t, l)
	assert.True(t, l.SameWorkerRetry, "still reported, so the log shows the repeat")
}

// TestRetryElsewhereFairness: a (queue, class) whose only pending task is
// excluded for the worker is not its candidate, so the worker gets other
// work instead of nothing, and the excluded task's queue and class are not
// charged for it.
func TestRetryElsewhereFairness(t *testing.T) {
	t.Parallel()
	f := newFixture(t, withGrace(grace))
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
	f := newFixture(t, withGrace(grace))
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
	assert.Equal(t, "transcribe task 1 failed after 2 attempt(s) (on workers "+a+", "+b+"): out of memory", got.Error)
}
