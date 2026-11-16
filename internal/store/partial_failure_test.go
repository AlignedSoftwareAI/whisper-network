package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

// failingJob sets up a five-task job whose task 1 fails for good on worker
// a while b and c hold tasks 2 and 3 (#63). It returns the job and b's and
// c's leases.
func (f *fixture) failingJob() (j *Job, a string, lb, lc *Lease) {
	f.t.Helper()
	a = f.worker(api.SandboxFull, f.pubID)
	b := f.worker(api.SandboxFull, f.pubID)
	c := f.worker(api.SandboxFull, f.pubID)
	j = f.job(f.pubID, "c", api.WeightMedium, 5, 10)
	la := f.claim(a)
	lb, lc = f.claim(b), f.claim(c)
	require.Equal(f.t, [3]int{1, 2, 3}, [3]int{la.Seq, lb.Seq, lc.Seq})

	end := f.fail(la, true) // max_attempts 1: task 1 is out of attempts
	assert.False(f.t, end.Requeued)
	assert.False(f.t, end.JobFailed, "siblings' leases are live")
	return j, a, lb, lc
}

// TestFailingJobKeepsLeasedSiblings: when one task fails for good, the
// job's leased tasks run to completion and keep their results, its pending
// tasks are cancelled and never leased, and the job fails when the last
// lease ends, with its progress in the error (#63, #65).
func TestFailingJobKeepsLeasedSiblings(t *testing.T) {
	t.Parallel()
	f := newFixture(t, maxAttempts(1))
	j, a, lb, lc := f.failingJob()

	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobRunning, got.State, "failing, not failed")
	assert.Equal(t, "transcribe task 1 failed after 1 attempt(s) (on worker "+a+"): whisper crashed", got.Error)
	assert.Equal(t, api.TaskCounts{Total: 5, Leased: 2, Failed: 1, Cancelled: 2}, got.Tasks)
	for _, seq := range []int{4, 5} {
		state, _ := f.task(j.ID, seq)
		assert.Equal(t, taskCancelled, state, "pending task %d", seq)
	}
	assert.Nil(t, f.claim(f.worker(api.SandboxFull, f.pubID)), "no new lease for a failing job")

	// b's lease is untouched and completes; its result is stored.
	_, err = f.s.Heartbeat(f.ctx, lb.ID, lb.WorkerID, ttl, LeaseAgeLimit{})
	require.NoError(t, err)
	end, err := f.s.CompleteTranscribe(f.ctx, lb.ID, lb.WorkerID, TranscribeCompletion{Result: []byte(`{"x":1}`)})
	require.NoError(t, err)
	assert.Equal(t, TranscribeEnd{}, end, "c's lease is still live")
	var result sql.NullString
	require.NoError(t, f.s.r.QueryRowContext(f.ctx, `SELECT result FROM tasks WHERE job_id = ? AND seq = 2`,
		j.ID).Scan(&result))
	assert.Equal(t, `{"x":1}`, result.String)

	// c releases (shutting down): its task is cancelled, not requeued, and
	// the job fails now that no lease is live.
	rel, err := f.s.Release(f.ctx, lc.ID, lc.WorkerID)
	require.NoError(t, err)
	assert.False(t, rel.Requeued)
	assert.True(t, rel.JobFailed)

	got, err = f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobFailed, got.State)
	assert.NotNil(t, got.CompletedAt)
	assert.Equal(t, "transcribe task 1 failed after 1 attempt(s) (on worker "+a+
		"): whisper crashed; 1 of 5 tasks done, 3 cancelled", got.Error)
	assert.Equal(t, api.TaskCounts{Total: 5, Done: 1, Failed: 1, Cancelled: 3}, got.Tasks)

	settled, err := f.s.SettleFailingJobs(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, settled, "already failed")
}

// TestFailingJobSiblingFailure: a leased sibling that fails while the job is
// failing is cancelled even with attempts left, and the first failure keeps
// naming the job's error.
func TestFailingJobSiblingFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t, maxAttempts(1))
	j, a, lb, lc := f.failingJob()
	f.s.cfg.MaxAttempts = 3 // b's failure alone would requeue

	end := f.fail(lb, true)
	assert.False(t, end.Requeued)
	assert.False(t, end.JobFailed)
	state, _ := f.task(j.ID, 2)
	assert.Equal(t, taskCancelled, state)

	end = f.fail(lc, false) // fails for good itself
	assert.True(t, end.JobFailed)
	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobFailed, got.State)
	assert.Equal(t, "transcribe task 1 failed after 1 attempt(s) (on worker "+a+
		"): whisper crashed; 0 of 5 tasks done, 3 cancelled", got.Error)
	assert.Equal(t, api.TaskCounts{Total: 5, Failed: 2, Cancelled: 3}, got.Tasks)
}

// TestDeleteFailingJobCancels: deleting a failing job still cancels its
// live leases at once.
func TestDeleteFailingJobCancels(t *testing.T) {
	t.Parallel()
	f := newFixture(t, maxAttempts(1))
	j, _, lb, lc := f.failingJob()
	cancelled, err := f.s.DeleteJob(f.ctx, j.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{lb.ID, lc.ID}, cancelled)
	_, err = f.s.Heartbeat(f.ctx, lb.ID, lb.WorkerID, ttl, LeaseAgeLimit{})
	assert.ErrorIs(t, err, ErrLeaseGone)
}

// TestSettleFailingJobsBackstop: a failing job left without live leases
// (here by ending them behind the store's back) is failed by the reaper's
// sweep.
func TestSettleFailingJobsBackstop(t *testing.T) {
	t.Parallel()
	f := newFixture(t, maxAttempts(1))
	j, _, _, _ := f.failingJob()
	_, err := f.s.w.ExecContext(f.ctx, `UPDATE leases SET ended_at = 1, outcome = 'cancelled' WHERE job_id = ?`, j.ID)
	require.NoError(t, err)
	settled, err := f.s.SettleFailingJobs(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{j.ID}, settled)
	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobFailed, got.State)
	assert.Contains(t, got.Error, "; 0 of 5 tasks done, 2 cancelled")
}

func (f *fixture) failStage(l *Lease, retryable bool, stage api.FailureStage) *LeaseEnd {
	f.t.Helper()
	require.NotNil(f.t, l)
	end, err := f.s.FailStage(f.ctx, l.ID, l.WorkerID, "no model", retryable, stage)
	require.NoError(f.t, err)
	return end
}

// TestPreWorkFailure: a pre-work failure requeues the task without using
// an attempt or excluding the worker, even if not retryable, but counts
// toward the worker's failure streak; repeated, it pauses the worker within
// one job and no job fails (#64).
func TestPreWorkFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t, maxAttempts(1), failureLimit(3))
	w := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)

	for i := range 3 {
		l := f.claim(w)
		require.NotNil(t, l, "the worker may take the task again (%d)", i)
		assert.Equal(t, 1, l.Attempt)
		end := f.failStage(l, i == 0, api.StagePreWork)
		assert.True(t, end.Requeued)
		assert.False(t, end.JobFailed)
		assert.Equal(t, 0, end.Attempt)
		state, attempts := f.task(j.ID, 1)
		assert.Equal(t, [2]any{"pending", 0}, [2]any{state, attempts})
		n, _ := f.streak(w)
		assert.Equal(t, i+1, n)
		if i < 2 {
			assert.Nil(t, end.AutoPaused)
		} else {
			require.NotNil(t, end.AutoPaused, "one job is enough for pre-work failures")
			assert.Equal(t, "3 consecutive failures in 1 job, the last before work started", end.AutoPaused.Reason)
		}
	}
	_, err := f.s.Claim(f.ctx, w, f.eligibility(w), ttl)
	assert.ErrorIs(t, err, ErrWorkerInactive)

	reason, err := f.s.RetryExclusion(f.ctx, j.ID, 1, w)
	require.NoError(t, err)
	assert.Empty(t, reason)

	// A healthy worker finishes the job.
	assert.True(t, f.complete(f.claim(f.worker(api.SandboxFull, f.pubID))))
}

// TestPreWorkFailureBreakerOff: without the breaker nothing would stop a
// worker failing the same task before work forever, so a pre-work failure
// counts as an ordinary one.
func TestPreWorkFailureBreakerOff(t *testing.T) {
	t.Parallel()
	f := newFixture(t, maxAttempts(1))
	w := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	end := f.failStage(f.claim(w), true, api.StagePreWork)
	assert.True(t, end.JobFailed)
	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobFailed, got.State)
}

// TestInputAndChildStagesCount: input and child failures behave as
// failures without a stage, and the stage is recorded on the lease.
func TestInputAndChildStagesCount(t *testing.T) {
	t.Parallel()
	for name, stage := range map[string]api.FailureStage{"input": api.StageInput, "child": api.StageChild, "none": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, maxAttempts(2), failureLimit(5))
			w := f.worker(api.SandboxFull, f.pubID)
			j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
			l := f.claim(w)
			end := f.failStage(l, true, stage)
			assert.True(t, end.Requeued)
			assert.Equal(t, 1, end.Attempt)
			assert.Nil(t, f.claim(w), "excluded from the task")
			_, attempts := f.task(j.ID, 1)
			assert.Equal(t, 1, attempts)
			var got sql.NullString
			require.NoError(t, f.s.r.QueryRowContext(f.ctx, `SELECT stage FROM leases WHERE id = ?`, l.ID).Scan(&got))
			assert.Equal(t, string(stage), got.String)
		})
	}
}
