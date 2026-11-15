package store

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

func failureLimit(n int) func(*Config) { return func(c *Config) { c.WorkerFailureLimit = n } }

func maxAttempts(n int) func(*Config) { return func(c *Config) { c.MaxAttempts = n } }

// streak returns a worker's failure streak and the distinct jobs in it.
func (f *fixture) streak(workerID string) (failures, jobs int) {
	f.t.Helper()
	w, err := f.s.WorkerByID(f.ctx, workerID)
	require.NoError(f.t, err)
	return w.FailureStreak, w.StreakJobs
}

func (f *fixture) fail(l *Lease, retryable bool) *LeaseEnd {
	f.t.Helper()
	require.NotNil(f.t, l)
	end, err := f.s.Fail(f.ctx, l.ID, l.WorkerID, "whisper crashed", retryable)
	require.NoError(f.t, err)
	return end
}

// workerPauses returns the worker.pause audit entries for a worker.
func (f *fixture) workerPauses(workerID string) []AuditEntry {
	f.t.Helper()
	es, err := f.s.QueryAudit(f.ctx, AuditFilter{ActionPrefix: "worker.pause", Target: WorkerPrincipal(workerID)})
	require.NoError(f.t, err)
	return es
}

// TestFailureStreakCounting: retryable failures and expiries extend the
// streak, a completion ends it, and non-retryable failures (the input's
// fault), releases and cancellations leave it alone.
func TestFailureStreakCounting(t *testing.T) {
	t.Parallel()
	f := newFixture(t, maxAttempts(10)) // the breaker is off: Config default
	w := f.worker(api.SandboxFull, f.pubID)
	// A worker never gets back a task it failed, so each claim below takes
	// another task of the same job.
	f.job(f.pubID, "c", api.WeightMedium, 4, 10)

	f.fail(f.claim(w), true)
	n, jobs := f.streak(w)
	assert.Equal(t, [2]int{1, 1}, [2]int{n, jobs}, "a retryable failure counts")

	f.claim(w)
	f.clk.Advance(ttl)
	_, err := f.s.ExpireLeases(f.ctx)
	require.NoError(t, err)
	n, jobs = f.streak(w)
	assert.Equal(t, [2]int{2, 1}, [2]int{n, jobs}, "an expiry counts; same job")

	_, err = f.s.Release(f.ctx, f.claim(w).ID, w)
	require.NoError(t, err)
	n, _ = f.streak(w)
	assert.Equal(t, 2, n, "a release counts as nothing")

	other := f.worker(api.SandboxFull, f.pubID)
	l := f.claim(w)
	cancelled, err := f.s.RemoveGrant(f.ctx, WorkerPrincipal(w), api.RoleProcess, QueueScope(f.pubID))
	require.NoError(t, err)
	require.Equal(t, []string{l.ID}, cancelled)
	n, _ = f.streak(w)
	assert.Equal(t, 2, n, "a cancellation counts as nothing")
	require.NoError(t, f.s.AddGrant(f.ctx, WorkerPrincipal(w), api.RoleProcess, QueueScope(f.pubID), "user:1"))

	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	l = f.claim(w)
	f.fail(l, false)
	n, jobs = f.streak(w)
	assert.Equal(t, [2]int{2, 1}, [2]int{n, jobs}, "a non-retryable failure neither counts nor resets")

	f.complete(f.claim(w))
	n, jobs = f.streak(w)
	assert.Equal(t, [2]int{0, 0}, [2]int{n, jobs}, "a completion ends the streak")
	n, _ = f.streak(other)
	assert.Zero(t, n)
}

// TestBreakerNeedsTwoJobs: one job failing task after task (a poisoned input)
// does not trip the breaker however long the streak; a second job does.
func TestBreakerNeedsTwoJobs(t *testing.T) {
	t.Parallel()
	f := newFixture(t, failureLimit(3), maxAttempts(10))
	w := f.worker(api.SandboxFull, f.pubID)
	// A worker never gets back a task it failed, so the poisoned job has a
	// task for each failure.
	poison := f.job(f.pubID, "c", api.WeightMedium, 4, 10)
	for range 4 {
		l := f.claim(w)
		require.Equal(t, poison.ID, l.JobID)
		assert.Nil(t, f.fail(l, true).AutoPaused)
	}
	n, jobs := f.streak(w)
	assert.Equal(t, [2]int{4, 1}, [2]int{n, jobs})
	wk, err := f.s.WorkerByID(f.ctx, w)
	require.NoError(t, err)
	assert.Equal(t, api.WorkerActive, wk.State)

	f.job(f.pubID, "c", api.WeightHigh, 1, 10)
	l := f.claim(w)
	require.NotEqual(t, poison.ID, l.JobID)
	end := f.fail(l, true)
	require.NotNil(t, end.AutoPaused)
	assert.Equal(t, AutoPause{WorkerID: w, Failures: 5, Jobs: 2, Reason: "5 consecutive failures across 2 jobs"},
		*end.AutoPaused)
}

// TestBreakerAutoPause: the lease end that trips the breaker pauses the
// worker, records why, and audits worker.pause by the system, all in its
// own transaction.
func TestBreakerAutoPause(t *testing.T) {
	t.Parallel()
	f := newFixture(t, failureLimit(3))
	w := f.worker(api.SandboxFull, f.pubID)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	ls := []*Lease{f.claim(w), f.claim(w), f.claim(w)}
	assert.Nil(t, f.fail(ls[0], true).AutoPaused)
	assert.Nil(t, f.fail(ls[1], true).AutoPaused)
	assert.Empty(t, f.workerPauses(w))

	end := f.fail(ls[2], true)
	require.NotNil(t, end.AutoPaused)
	assert.Equal(t, "3 consecutive failures across 3 jobs", end.AutoPaused.Reason)
	assert.True(t, end.Requeued, "the task itself is retried as usual")

	wk, err := f.s.WorkerByID(f.ctx, w)
	require.NoError(t, err)
	assert.Equal(t, api.WorkerPaused, wk.State)
	assert.Equal(t, SystemActor, wk.DecidedBy)
	assert.Equal(t, f.clk.Now(), *wk.DecidedAt)
	assert.Equal(t, "3 consecutive failures across 3 jobs", wk.PauseReason)

	es := f.workerPauses(w)
	require.Len(t, es, 1)
	assert.Equal(t, SystemActor, es[0].Actor)
	var d map[string]any
	require.NoError(t, json.Unmarshal([]byte(es[0].Detail), &d))
	assert.Equal(t, map[string]any{"from": "active", "cancelled_leases": 0.0, "reason": wk.PauseReason,
		"failures": 3.0, "jobs": 3.0}, d)
}

// TestBreakerAuditFailureRollsBack: if the audit entry cannot be written,
// nothing the lease end did is kept: the lease is still live and the worker
// still active.
func TestBreakerAuditFailureRollsBack(t *testing.T) {
	t.Parallel()
	f := newFixture(t, failureLimit(2))
	w := f.worker(api.SandboxFull, f.pubID)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	l1, l2 := f.claim(w), f.claim(w)
	f.fail(l1, true)
	_, err := f.s.w.ExecContext(f.ctx, `CREATE TRIGGER no_audit BEFORE INSERT ON audit
		BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`)
	require.NoError(t, err)

	_, err = f.s.Fail(f.ctx, l2.ID, w, "crashed", true)
	require.ErrorContains(t, err, "audit unavailable")
	wk, err := f.s.WorkerByID(f.ctx, w)
	require.NoError(t, err)
	assert.Equal(t, api.WorkerActive, wk.State)
	assert.Empty(t, wk.PauseReason)
	assert.Equal(t, 1, wk.FailureStreak)
	_, err = f.s.Heartbeat(f.ctx, l2.ID, w, ttl, LeaseAgeLimit{})
	assert.NoError(t, err, "the lease is still live")
}

// TestBreakerPausedWorkerFinishesCurrentLeases: a worker paused by the
// breaker keeps its other live leases (heartbeats and results are accepted)
// but claims nothing new.
func TestBreakerPausedWorkerFinishesCurrentLeases(t *testing.T) {
	t.Parallel()
	f := newFixture(t, failureLimit(2))
	w := f.worker(api.SandboxFull, f.pubID)
	for range 4 {
		f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	}
	l1, l2, l3 := f.claim(w), f.claim(w), f.claim(w)
	f.fail(l1, true)
	require.NotNil(t, f.fail(l2, true).AutoPaused)

	_, err := f.s.Heartbeat(f.ctx, l3.ID, w, ttl, LeaseAgeLimit{})
	require.NoError(t, err)
	f.complete(l3)
	_, err = f.s.Claim(f.ctx, w, f.eligibility(w), ttl)
	require.ErrorIs(t, err, ErrWorkerInactive)

	wk, err := f.s.WorkerByID(f.ctx, w)
	require.NoError(t, err)
	assert.Equal(t, api.WorkerPaused, wk.State, "a completion while paused does not resume it")
	assert.NotEmpty(t, wk.PauseReason)
	assert.Zero(t, wk.FailureStreak, "but it does end the streak")
}

// TestBreakerOnlyActiveWorkers: further failures of a worker that is
// already paused (by the breaker or an administrator) pause nothing again.
func TestBreakerOnlyActiveWorkers(t *testing.T) {
	t.Parallel()
	f := newFixture(t, failureLimit(2))
	w := f.worker(api.SandboxFull, f.pubID)
	for range 3 {
		f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	}
	ls := []*Lease{f.claim(w), f.claim(w), f.claim(w)}
	_, err := f.s.SetWorkerState(f.ctx, w, api.WorkerPaused, "user:1")
	require.NoError(t, err)
	for _, l := range ls {
		assert.Nil(t, f.fail(l, true).AutoPaused)
	}
	wk, err := f.s.WorkerByID(f.ctx, w)
	require.NoError(t, err)
	assert.Equal(t, "user:1", wk.DecidedBy)
	assert.Empty(t, wk.PauseReason)
	assert.Equal(t, 3, wk.FailureStreak)
	assert.Empty(t, f.workerPauses(w), "the store audits only its own pause")
}

// TestBreakerExpiries: leases expiring together in one reaper pass pause
// their worker once.
func TestBreakerExpiries(t *testing.T) {
	t.Parallel()
	f := newFixture(t, failureLimit(2))
	w := f.worker(api.SandboxFull, f.pubID)
	for range 3 {
		f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	}
	f.claim(w)
	f.claim(w)
	f.claim(w)
	f.clk.Advance(ttl)
	ends, err := f.s.ExpireLeases(f.ctx)
	require.NoError(t, err)
	require.Len(t, ends, 3)
	var paused []*AutoPause
	for _, e := range ends {
		if e.AutoPaused != nil {
			paused = append(paused, e.AutoPaused)
		}
	}
	require.Len(t, paused, 1)
	assert.Equal(t, "2 consecutive failures across 2 jobs", paused[0].Reason)
	assert.Len(t, f.workerPauses(w), 1)
}

// TestBreakerResumeClears: resuming ends the streak and the pause reason,
// so the worker gets a full allowance again.
func TestBreakerResumeClears(t *testing.T) {
	t.Parallel()
	f := newFixture(t, failureLimit(2))
	w := f.worker(api.SandboxFull, f.pubID)
	for range 4 {
		f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	}
	f.fail(f.claim(w), true)
	require.NotNil(t, f.fail(f.claim(w), true).AutoPaused)

	_, err := f.s.SetWorkerState(f.ctx, w, api.WorkerActive, "user:1")
	require.NoError(t, err)
	wk, err := f.s.WorkerByID(f.ctx, w)
	require.NoError(t, err)
	assert.Empty(t, wk.PauseReason)
	assert.Zero(t, wk.FailureStreak)
	assert.Zero(t, wk.StreakJobs)

	assert.Nil(t, f.fail(f.claim(w), true).AutoPaused, "one failure after the resume is not a streak of two")
	require.NotNil(t, f.fail(f.claim(w), true).AutoPaused)
}

// TestBreakerDisabled: with no limit the streak is still kept (for the
// Workers page) but never pauses anyone.
func TestBreakerDisabled(t *testing.T) {
	t.Parallel()
	f := newFixture(t, failureLimit(0))
	w := f.worker(api.SandboxFull, f.pubID)
	for range 5 {
		f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	}
	for range 5 {
		assert.Nil(t, f.fail(f.claim(w), true).AutoPaused)
	}
	wk, err := f.s.WorkerByID(f.ctx, w)
	require.NoError(t, err)
	assert.Equal(t, api.WorkerActive, wk.State)
	assert.Equal(t, 5, wk.FailureStreak)
	assert.Equal(t, 5, wk.StreakJobs)
}

// TestFailureStreakGoesWithJob: deleting a job drops its part of the streak.
func TestFailureStreakGoesWithJob(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	f.fail(f.claim(w), true)
	f.fail(f.claim(w), true)
	_, err := f.s.DeleteJob(f.ctx, j.ID)
	require.NoError(t, err)
	n, jobs := f.streak(w)
	assert.Equal(t, [2]int{1, 1}, [2]int{n, jobs})
}
