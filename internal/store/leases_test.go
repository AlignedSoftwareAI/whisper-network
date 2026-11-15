package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

func TestHeartbeat(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	other := f.worker(api.SandboxFull, f.pubID)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	l := f.claim(w)

	f.clk.Advance(60 * time.Second)
	exp, err := f.s.Heartbeat(f.ctx, l.ID, w, ttl, LeaseAgeLimit{})
	require.NoError(t, err)
	assert.Equal(t, f.clk.Now().Add(ttl), exp)

	_, err = f.s.Heartbeat(f.ctx, l.ID, other, ttl, LeaseAgeLimit{})
	assert.ErrorIs(t, err, ErrNotFound, "another worker's lease is invisible")
	_, err = f.s.Heartbeat(f.ctx, "lse_missing", w, ttl, LeaseAgeLimit{})
	assert.ErrorIs(t, err, ErrNotFound)

	f.clk.Advance(ttl)
	_, err = f.s.Heartbeat(f.ctx, l.ID, w, ttl, LeaseAgeLimit{})
	assert.ErrorIs(t, err, ErrLeaseGone, "expired, even before the reaper runs")
}

func TestExpiryRequeueAndMaxAttempts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	w2 := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 2, 10)

	l1 := f.claim(w)
	require.Equal(t, 1, l1.Seq)
	l2 := f.claim(w) // sibling, stays healthy via heartbeats
	f.clk.Advance(ttl - time.Second)
	ends, err := f.s.ExpireLeases(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, ends, "not yet expired")

	_, err = f.s.Heartbeat(f.ctx, l2.ID, w, ttl, LeaseAgeLimit{})
	require.NoError(t, err)
	f.clk.Advance(time.Second)
	ends, err = f.s.ExpireLeases(f.ctx)
	require.NoError(t, err)
	require.Len(t, ends, 1)
	assert.Equal(t, LeaseEnd{LeaseID: l1.ID, JobID: j.ID, Seq: 1, Kind: api.KindTranscribe, WorkerID: w,
		Outcome: api.OutcomeExpired, Attempt: 1, Requeued: true}, ends[0])
	state, attempts := f.task(j.ID, 1)
	assert.Equal(t, "pending", state)
	assert.Equal(t, 1, attempts)

	// Second attempt, on another worker (w never gets the task back), also
	// expires: max_attempts (2) reached, job fails and the sibling's lease
	// is cancelled.
	assert.Nil(t, f.claim(w))
	l3 := f.claim(w2)
	require.Equal(t, 1, l3.Seq)
	assert.Equal(t, 2, l3.Attempt)
	f.clk.Advance(ttl / 2)
	_, err = f.s.Heartbeat(f.ctx, l2.ID, w, ttl, LeaseAgeLimit{})
	require.NoError(t, err)
	f.clk.Advance(ttl / 2)
	ends, err = f.s.ExpireLeases(f.ctx)
	require.NoError(t, err)
	require.Len(t, ends, 1)
	assert.True(t, ends[0].JobFailed)
	assert.False(t, ends[0].Requeued)
	assert.Equal(t, []string{l2.ID}, ends[0].Cancelled)

	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobFailed, got.State)
	assert.Contains(t, got.Error, "2 attempt(s)")
	assert.Equal(t, 1, got.Tasks.Failed)
	_, err = f.s.Heartbeat(f.ctx, l2.ID, w, ttl, LeaseAgeLimit{})
	assert.ErrorIs(t, err, ErrLeaseGone)
	assert.Nil(t, f.claim(w))
}

func TestFencingExpiredLeaseCannotComplete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	slow := f.worker(api.SandboxFull, f.pubID)
	fast := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	old := f.claim(slow)

	f.clk.Advance(ttl)
	res := TranscribeCompletion{Result: []byte(`{"segments":[{"text":"stale"}]}`)}
	_, err := f.s.CompleteTranscribe(f.ctx, old.ID, slow, res)
	assert.ErrorIs(t, err, ErrLeaseGone, "expired but not yet reaped")

	_, err = f.s.ExpireLeases(f.ctx)
	require.NoError(t, err)
	fresh := f.claim(fast)
	require.NotNil(t, fresh)
	assert.True(t, f.complete(fresh))

	_, err = f.s.CompleteTranscribe(f.ctx, old.ID, slow, res)
	assert.ErrorIs(t, err, ErrLeaseGone)
	out, err := f.s.JobOutputs(f.ctx, j.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"segments":[]}`, string(out.Transcribe[0].Result), "stale result never stored")
	assert.Equal(t, fast, out.Transcribe[0].WorkerID)
}

func TestCompleteIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	l := f.claim(w)
	res := TranscribeCompletion{Result: []byte(`{"segments": [ ]}`), ComputeMS: 5}
	done, err := f.s.CompleteTranscribe(f.ctx, l.ID, w, res)
	require.NoError(t, err)
	assert.True(t, done)

	f.clk.Advance(time.Hour) // long past expiry; the retry still succeeds
	res.Result = []byte(`{"segments":[]}`)
	done, err = f.s.CompleteTranscribe(f.ctx, l.ID, w, res)
	require.NoError(t, err, "same result (modulo whitespace) again")
	assert.True(t, done)

	res.Result = []byte(`{"segments":[{"text":"x"}]}`)
	_, err = f.s.CompleteTranscribe(f.ctx, l.ID, w, res)
	assert.ErrorIs(t, err, ErrConflict)

	res.Result = []byte(`not json`)
	_, err = f.s.CompleteTranscribe(f.ctx, l.ID, w, res)
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestReleaseDoesNotConsumeAttempt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	for range 5 {
		l := f.claim(w)
		require.NotNil(t, l)
		end, err := f.s.Release(f.ctx, l.ID, w)
		require.NoError(t, err)
		assert.True(t, end.Requeued)
		assert.Equal(t, 0, end.Attempt)
		_, attempts := f.task(j.ID, 1)
		assert.Equal(t, 0, attempts)
	}
	l := f.claim(w)
	_, err := f.s.Release(f.ctx, l.ID, w)
	require.NoError(t, err)
	_, err = f.s.Release(f.ctx, l.ID, w)
	assert.ErrorIs(t, err, ErrLeaseGone, "a released lease is gone")
}

func TestFail(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	w2 := f.worker(api.SandboxFull, f.pubID)
	j1 := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	l := f.claim(w)
	end, err := f.s.Fail(f.ctx, l.ID, w, "whisper crashed", true)
	require.NoError(t, err)
	assert.True(t, end.Requeued)
	assert.Equal(t, api.OutcomeFailed, end.Outcome)

	assert.Nil(t, f.claim(w), "w never retries a task it failed")
	l = f.claim(w2)
	end, err = f.s.Fail(f.ctx, l.ID, w2, "whisper crashed", true)
	require.NoError(t, err)
	assert.True(t, end.JobFailed, "retryable but out of attempts")

	j2 := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	l = f.claim(w)
	require.Equal(t, j2.ID, l.JobID)
	end, err = f.s.Fail(f.ctx, l.ID, w, "result invalid", false)
	require.NoError(t, err)
	assert.True(t, end.JobFailed, "non-retryable fails at once")
	assert.Equal(t, 1, end.Attempt)

	for _, id := range []string{j1.ID, j2.ID} {
		got, err := f.s.Job(f.ctx, id)
		require.NoError(t, err)
		assert.Equal(t, api.JobFailed, got.State)
	}
	_, err = f.s.Fail(f.ctx, l.ID, w, "again", true)
	assert.ErrorIs(t, err, ErrLeaseGone)
}

func TestPrepareFlow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	weak := f.worker(api.SandboxFSOnly, f.pubID)
	strong := f.worker(api.SandboxFull, f.pubID)
	j := f.prepJob(f.pubID, "c")

	el := f.eligibility(weak)
	el.SandboxLevel = api.SandboxFSOnly
	l, err := f.s.Claim(f.ctx, weak, el, ttl)
	require.NoError(t, err)
	assert.Nil(t, l, "prepare needs a full sandbox by default")

	l = f.claim(strong)
	require.NotNil(t, l)
	assert.Equal(t, api.KindPrepare, l.Kind)
	assert.Equal(t, 0, l.Seq)
	wire := l.API(30 * time.Second)
	assert.Equal(t, int64(1000), wire.InputBytes)
	assert.Equal(t, "orig", wire.InputSHA256)
	assert.Equal(t, "/lease/"+l.ID+"/input", wire.InputURL)
	assert.Equal(t, 30, wire.HeartbeatIntervalS)
	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobPreparing, got.State, "prepare phase stays preparing")
	assert.NotNil(t, got.StartedAt)

	_, err = f.s.CompleteTranscribe(f.ctx, l.ID, strong, TranscribeCompletion{Result: []byte(`{}`)})
	assert.ErrorIs(t, err, ErrInvalid, "wrong kind")

	samples := int64(7200 * canonicalSampleRate)
	p := PrepareCompletion{Canonical: Canonical{Bytes: 500, SHA256: "canon", Samples: samples},
		Ranges:    []SampleRange{{0, samples / 2}, {samples / 2, samples}},
		ComputeMS: 4000, FFmpegVersion: "7.1.1"}
	require.NoError(t, f.s.AcceptPrepare(f.ctx, l.ID, strong, p))
	require.NoError(t, f.s.AcceptPrepare(f.ctx, l.ID, strong, p), "idempotent")
	p2 := p
	p2.Canonical.SHA256 = "different"
	assert.ErrorIs(t, f.s.AcceptPrepare(f.ctx, l.ID, strong, p2), ErrConflict)

	got, err = f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobQueued, got.State)
	require.NotNil(t, got.Canonical)
	assert.Equal(t, samples, got.Canonical.Samples)
	assert.Equal(t, api.TaskCounts{Total: 3, Pending: 2, Done: 1}, got.Tasks)

	t1 := f.claim(weak) // transcribe goes to any sandbox level
	require.NotNil(t, t1)
	assert.Equal(t, api.KindTranscribe, t1.Kind)
	assert.Equal(t, samples/2, t1.API(time.Second).AudioSamples)
	got, err = f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobRunning, got.State)

	t2 := f.claim(strong)
	assert.False(t, f.complete(t1))
	assert.True(t, f.complete(t2))
	out, err := f.s.JobOutputs(f.ctx, j.ID)
	require.NoError(t, err)
	require.NotNil(t, out.Prepare)
	assert.Equal(t, "7.1.1", out.Prepare.EngineVersion)
	assert.Len(t, out.Transcribe, 2)
	done, err := f.s.MarkJobDone(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(6000), done.ComputeMS, "prepare compute included")
}

func TestAcceptPrepareRequiresLiveLease(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	f.prepJob(f.pubID, "c")
	l := f.claim(w)
	f.clk.Advance(ttl)
	err := f.s.AcceptPrepare(f.ctx, l.ID, w, PrepareCompletion{Canonical: Canonical{Samples: 10},
		Ranges: []SampleRange{{0, 10}}})
	assert.ErrorIs(t, err, ErrLeaseGone)
}

func TestExtendLiveLeases(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	l := f.claim(w)
	f.clk.Advance(time.Hour) // server was down past the expiry
	n, err := f.s.ExtendLiveLeases(f.ctx, ttl)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	ends, err := f.s.ExpireLeases(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, ends)
	got, err := f.s.Lease(f.ctx, l.ID, w)
	require.NoError(t, err)
	assert.True(t, got.Live)
	assert.Equal(t, f.clk.Now().Add(ttl), got.ExpiresAt)
}
