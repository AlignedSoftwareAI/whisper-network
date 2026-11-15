package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

func TestWorkerLifecycle(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	facts := api.WorkerFacts{Hostname: "nuc", Cores: 4, SandboxLevel: api.SandboxFull,
		Kinds: []api.TaskKind{api.KindTranscribe}, Models: []string{"small.en"}}
	w, err := f.s.RegisterWorker(f.ctx, "wt", facts)
	require.NoError(t, err)
	assert.Equal(t, api.WorkerPending, w.State)

	n, err := f.s.CountPendingWorkers(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, err = f.s.Claim(f.ctx, w.ID, f.eligibility(w.ID), ttl)
	assert.ErrorIs(t, err, ErrWorkerInactive)
	_, err = f.s.SetWorkerState(f.ctx, w.ID, api.WorkerPaused, "user:1")
	assert.ErrorIs(t, err, ErrConflict, "pending cannot pause")

	_, err = f.s.SetWorkerState(f.ctx, w.ID, api.WorkerActive, "user:1")
	require.NoError(t, err)
	facts.Cores = 8
	require.NoError(t, f.s.UpdateWorkerFacts(f.ctx, w.ID, facts))
	require.NoError(t, f.s.TouchWorker(f.ctx, w.ID, &api.Gauges{SlotsBusy: 1}))

	got, err := f.s.WorkerByTokenHash(f.ctx, "wt")
	require.NoError(t, err)
	assert.Equal(t, api.WorkerActive, got.State)
	assert.Equal(t, 8, got.Facts.Cores)
	assert.Equal(t, []string{"small.en"}, got.Facts.Models)
	assert.Equal(t, "user:1", got.DecidedBy)
	assert.NotNil(t, got.LastSeenAt)
	assert.JSONEq(t, `{"slots_busy":1,"child_rss_bytes":0,"load1":0,"cpu_throttled_seconds":0}`, string(got.Gauges))

	_, err = f.s.SetWorkerState(f.ctx, w.ID, api.WorkerRejected, "user:1")
	require.NoError(t, err)
	_, err = f.s.WorkerByTokenHash(f.ctx, "wt")
	assert.ErrorIs(t, err, ErrNotFound, "rejected token is revoked")
	_, err = f.s.SetWorkerState(f.ctx, w.ID, api.WorkerActive, "user:1")
	assert.ErrorIs(t, err, ErrConflict, "rejected is terminal")
}

func TestRejectCancelsLeasesWithoutConsumingAttempt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c1", api.WeightMedium, 1, 60)
	l := f.claim(w)
	require.NotNil(t, l)

	cancelled, err := f.s.SetWorkerState(f.ctx, w, api.WorkerRejected, "user:1")
	require.NoError(t, err)
	assert.Equal(t, []string{l.ID}, cancelled)
	state, attempts := f.task(j.ID, 1)
	assert.Equal(t, "pending", state)
	assert.Equal(t, 0, attempts)
	_, err = f.s.Heartbeat(f.ctx, l.ID, w, ttl, LeaseAgeLimit{})
	assert.ErrorIs(t, err, ErrLeaseGone)
}

func TestDeletePendingWorkersUnseen(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	old, err := f.s.RegisterWorker(f.ctx, "a", api.WorkerFacts{})
	require.NoError(t, err)
	require.NoError(t, f.s.AddGrant(f.ctx, old.Principal(), api.RoleProcess, QueueScope(f.pubID), "user:1"))
	f.clk.Advance(8 * 24 * time.Hour)
	_, err = f.s.RegisterWorker(f.ctx, "b", api.WorkerFacts{})
	require.NoError(t, err)
	active := f.worker(api.SandboxNone) // registered now, active: never reaped

	n, err := f.s.DeletePendingWorkersUnseen(f.ctx, f.clk.Now().Add(-7*24*time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	_, err = f.s.WorkerByID(f.ctx, old.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	gs, err := f.s.GrantsFor(f.ctx, old.Principal())
	require.NoError(t, err)
	assert.Empty(t, gs, "grants of deleted workers go too")

	ws, err := f.s.ListWorkers(f.ctx)
	require.NoError(t, err)
	require.Len(t, ws, 2)
	assert.Equal(t, api.WorkerPending, ws[0].State, "pending first")
	assert.Equal(t, active, ws[1].ID)
}

func TestProcessQueuesWildcard(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	other := f.queue("other", 0)
	w := f.worker(api.SandboxFull)
	require.NoError(t, f.s.AddGrant(f.ctx, WorkerPrincipal(w), api.RoleProcess, ScopeAll, "user:1"))
	qs, err := f.s.ProcessQueues(f.ctx, w)
	require.NoError(t, err)
	assert.Equal(t, []int64{f.pubID, other}, qs)
}
