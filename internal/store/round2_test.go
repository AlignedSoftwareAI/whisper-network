package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

// TestRemoveProcessGrantCancelsLeases: removing a worker's process grant
// on a queue cancels its live leases there in the same transaction (the
// task requeues, the attempt is refunded) and leaves leases on queues it
// still holds alone. A "*" grant covers every queue.
func TestRemoveProcessGrantCancelsLeases(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	other := f.queue("other", 100)
	w := f.worker(api.SandboxFull, f.pubID, other)
	jp := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	jo := f.job(other, "c", api.WeightMedium, 1, 10)
	l1, l2 := f.claim(w), f.claim(w)
	onPub, onOther := l1, l2
	if l1.QueueID != f.pubID {
		onPub, onOther = l2, l1
	}

	cancelled, err := f.s.RemoveGrant(f.ctx, WorkerPrincipal(w), api.RoleProcess, QueueScope(f.pubID))
	require.NoError(t, err)
	assert.Equal(t, []string{onPub.ID}, cancelled)
	got, err := f.s.Lease(f.ctx, onPub.ID, w)
	require.NoError(t, err)
	assert.Equal(t, api.OutcomeCancelled, got.Outcome)
	state, attempts := f.task(jp.ID, 1)
	assert.Equal(t, "pending", state)
	assert.Zero(t, attempts, "the attempt is refunded")
	_, err = f.s.Heartbeat(f.ctx, onPub.ID, w, ttl, LeaseAgeLimit{})
	assert.ErrorIs(t, err, ErrLeaseGone)
	_, err = f.s.Heartbeat(f.ctx, onOther.ID, w, ttl, LeaseAgeLimit{})
	require.NoError(t, err, "the lease on a queue still granted lives on")
	state, _ = f.task(jo.ID, 1)
	assert.Equal(t, "leased", state)

	// "*" covers every queue: removing a narrower grant then cancels nothing,
	// removing "*" cancels what no remaining grant covers.
	require.NoError(t, f.s.AddGrant(f.ctx, WorkerPrincipal(w), api.RoleProcess, "*", "user:1"))
	cancelled, err = f.s.RemoveGrant(f.ctx, WorkerPrincipal(w), api.RoleProcess, QueueScope(other))
	require.NoError(t, err)
	assert.Empty(t, cancelled)
	cancelled, err = f.s.RemoveGrant(f.ctx, WorkerPrincipal(w), api.RoleProcess, "*")
	require.NoError(t, err)
	assert.Equal(t, []string{onOther.ID}, cancelled)

	// Other roles and principals never touch leases.
	c := "client:x"
	require.NoError(t, f.s.AddGrant(f.ctx, c, api.RoleSubmit, QueueScope(f.pubID), "user:1"))
	cancelled, err = f.s.RemoveGrant(f.ctx, c, api.RoleSubmit, QueueScope(f.pubID))
	require.NoError(t, err)
	assert.Empty(t, cancelled)
}

// TestClaimReadsGrantsInTransaction reproduces the original issue: the
// claim's queue list was read before its transaction, so a grant revoked
// during a long-poll was still honoured. Eligibility no longer carries
// queues; Claim reads them inside its transaction.
func TestClaimReadsGrantsInTransaction(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	el := f.eligibility(w) // computed before the revocation, as a long-poll does
	f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	_, err := f.s.RemoveGrant(f.ctx, WorkerPrincipal(w), api.RoleProcess, QueueScope(f.pubID))
	require.NoError(t, err)
	l, err := f.s.Claim(f.ctx, w, el, ttl)
	require.NoError(t, err)
	assert.Nil(t, l)

	require.NoError(t, f.s.AddGrant(f.ctx, WorkerPrincipal(w), api.RoleProcess, "*", "user:1"))
	l, err = f.s.Claim(f.ctx, w, el, ttl)
	require.NoError(t, err)
	assert.NotNil(t, l, `"*" covers every queue`)
}

// TestHeartbeatRefusals: a heartbeat does not extend a lease whose worker
// is neither active nor paused, whose worker lost process on its queue by
// any path, or which is older than its age limit.
func TestHeartbeatRefusals(t *testing.T) {
	t.Parallel()

	t.Run("worker state", func(t *testing.T) {
		f := newFixture(t)
		w := f.worker(api.SandboxFull, f.pubID)
		f.job(f.pubID, "c", api.WeightMedium, 1, 10)
		l := f.claim(w)
		_, err := f.s.SetWorkerState(f.ctx, w, api.WorkerPaused, "user:1")
		require.NoError(t, err)
		_, err = f.s.Heartbeat(f.ctx, l.ID, w, ttl, LeaseAgeLimit{})
		require.NoError(t, err, "a paused worker finishes what it holds")
		// Not through SetWorkerState (which would cancel the lease): the
		// heartbeat check stands on its own.
		_, err = f.s.w.ExecContext(f.ctx, `UPDATE workers SET state = 'pending' WHERE id = ?`, w)
		require.NoError(t, err)
		_, err = f.s.Heartbeat(f.ctx, l.ID, w, ttl, LeaseAgeLimit{})
		assert.ErrorIs(t, err, ErrLeaseGone)
	})

	t.Run("grant", func(t *testing.T) {
		f := newFixture(t)
		w := f.worker(api.SandboxFull, f.pubID)
		f.job(f.pubID, "c", api.WeightMedium, 1, 10)
		l := f.claim(w)
		_, err := f.s.w.ExecContext(f.ctx, `DELETE FROM grants WHERE principal = ?`, WorkerPrincipal(w))
		require.NoError(t, err)
		_, err = f.s.Heartbeat(f.ctx, l.ID, w, ttl, LeaseAgeLimit{})
		assert.ErrorIs(t, err, ErrLeaseGone)
	})

	t.Run("age", func(t *testing.T) {
		f := newFixture(t)
		w := f.worker(api.SandboxFull, f.pubID)
		f.job(f.pubID, "c", api.WeightMedium, 1, 3600) // cost 3600 (small.en factor 1)
		l := f.claim(w)
		age := LeaseAgeLimit{Min: time.Hour, PerCost: 2} // max(1h, 2 × 3600 s) = 2h
		for range 2*60 - 1 {
			f.clk.Advance(time.Minute)
			_, err := f.s.Heartbeat(f.ctx, l.ID, w, ttl, age)
			require.NoError(t, err)
		}
		f.clk.Advance(time.Minute)
		_, err := f.s.Heartbeat(f.ctx, l.ID, w, ttl, age)
		assert.ErrorIs(t, err, ErrLeaseGone, "two hours old")
		f.clk.Advance(ttl)
		ends, err := f.s.ExpireLeases(f.ctx)
		require.NoError(t, err)
		assert.Len(t, ends, 1, "then it simply expires")
	})
}

func TestLeaseAgeLimit(t *testing.T) {
	t.Parallel()
	assert.Zero(t, LeaseAgeLimit{}.of(1e6), "zero means no limit")
	assert.Equal(t, 6*time.Hour, LeaseAgeLimit{Min: 6 * time.Hour, PerCost: 4}.of(60))
	assert.Equal(t, 12*time.Hour, LeaseAgeLimit{Min: 6 * time.Hour, PerCost: 4}.of(3*3600))
}

// TestPendingPerSource: one source address may hold only PerSource pending
// registrations; other sources, and workers once approved, do not count.
func TestPendingPerSource(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	lim := PendingLimits{Total: 10, PerSource: 2, Source: "192.0.2.1"}
	a, err := f.s.RegisterWorkerCapped(f.ctx, "a", api.WorkerFacts{}, lim)
	require.NoError(t, err)
	_, err = f.s.RegisterWorkerCapped(f.ctx, "b", api.WorkerFacts{}, lim)
	require.NoError(t, err)
	_, err = f.s.RegisterWorkerCapped(f.ctx, "c", api.WorkerFacts{}, lim)
	assert.ErrorIs(t, err, ErrPendingSourceFull)

	elsewhere := lim
	elsewhere.Source = "2001:db8::/64"
	_, err = f.s.RegisterWorkerCapped(f.ctx, "c", api.WorkerFacts{}, elsewhere)
	require.NoError(t, err)

	_, err = f.s.SetWorkerState(f.ctx, a.ID, api.WorkerActive, "user:1")
	require.NoError(t, err)
	_, err = f.s.RegisterWorkerCapped(f.ctx, "d", api.WorkerFacts{}, lim)
	require.NoError(t, err, "approving one frees its source's slot")

	var from string
	require.NoError(t, f.s.r.QueryRowContext(f.ctx, `SELECT registered_from FROM workers WHERE id = ?`, a.ID).Scan(&from))
	assert.Equal(t, "192.0.2.1", from)
}

// TestPrunePendingDeletesScopedGrants reproduces the original issue:
// pruning a pending worker deleted the grants it held but left grants
// scoped to it (worker_admin on worker:<id>), unlike rejected pruning.
func TestPrunePendingDeletesScopedGrants(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w, err := f.s.RegisterWorker(f.ctx, "a", api.WorkerFacts{})
	require.NoError(t, err)
	require.NoError(t, f.s.AddGrant(f.ctx, "user:7", api.RoleWorkerAdmin, WorkerScope(w.ID), "user:1"))
	f.clk.Advance(8 * 24 * time.Hour)
	n, err := f.s.DeletePendingWorkersUnseen(f.ctx, f.clk.Now().Add(-7*24*time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	gs, err := f.s.GrantsFor(f.ctx, "user:7")
	require.NoError(t, err)
	assert.Empty(t, gs)
}
