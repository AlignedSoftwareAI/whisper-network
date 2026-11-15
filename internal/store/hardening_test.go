package store

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

// TestDatabaseFilesArePrivate: the database and its WAL files hold token
// and password hashes, so they are created 0600, and an existing
// world-readable database is tightened on open.
func TestDatabaseFilesArePrivate(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes do not apply on Windows")
	}
	f := newFixture(t)
	f.worker(api.SandboxFull) // a write, so the WAL files exist
	for _, p := range []string{f.path, f.path + "-wal", f.path + "-shm"} {
		fi, err := os.Stat(p)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), p)
	}

	legacy := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(f.ctx, Config{Path: legacy})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, os.Chmod(legacy, 0o644))
	s, err = Open(f.ctx, Config{Path: legacy})
	require.NoError(t, err)
	defer s.Close()
	fi, err := os.Stat(legacy)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

// TestPendingExpiryCountsFromRegistration: a pending worker that keeps
// polling (and so keeps last_seen_at fresh) still expires TTL after it
// registered.
func TestPendingExpiryCountsFromRegistration(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w, err := f.s.RegisterWorker(f.ctx, "a", api.WorkerFacts{})
	require.NoError(t, err)
	for range 8 {
		f.clk.Advance(24 * time.Hour)
		require.NoError(t, f.s.TouchWorker(f.ctx, w.ID, nil))
	}
	n, err := f.s.DeletePendingWorkersUnseen(f.ctx, f.clk.Now().Add(-7*24*time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	_, err = f.s.WorkerByID(f.ctx, w.ID)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRegisterWorkerCapped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a, err := f.s.RegisterWorkerCapped(f.ctx, "a", api.WorkerFacts{}, PendingLimits{Total: 2})
	require.NoError(t, err)
	_, err = f.s.RegisterWorkerCapped(f.ctx, "b", api.WorkerFacts{}, PendingLimits{Total: 2})
	require.NoError(t, err)
	_, err = f.s.RegisterWorkerCapped(f.ctx, "c", api.WorkerFacts{}, PendingLimits{Total: 2})
	assert.ErrorIs(t, err, ErrPendingFull)
	f.worker(api.SandboxFull) // active workers do not count
	_, err = f.s.SetWorkerState(f.ctx, a.ID, api.WorkerActive, "user:1")
	require.NoError(t, err)
	_, err = f.s.RegisterWorkerCapped(f.ctx, "c", api.WorkerFacts{}, PendingLimits{Total: 2})
	assert.NoError(t, err, "approving one frees a slot")
	_, err = f.s.RegisterWorkerCapped(f.ctx, "d", api.WorkerFacts{}, PendingLimits{})
	assert.NoError(t, err, "zero means uncapped")
}

// TestRegisterWorkerCappedConcurrent: the count and the insert are one
// transaction, so racing registrations never overshoot the cap.
func TestRegisterWorkerCappedConcurrent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const cap = 5
	var ok, full atomic.Int64
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			_, err := f.s.RegisterWorkerCapped(f.ctx, "tok-"+string(rune('A'+i)), api.WorkerFacts{}, PendingLimits{Total: cap})
			switch {
			case err == nil:
				ok.Add(1)
			case assert.ErrorIs(t, err, ErrPendingFull):
				full.Add(1)
			}
		})
	}
	wg.Wait()
	assert.EqualValues(t, cap, ok.Load())
	assert.EqualValues(t, 40-cap, full.Load())
	n, err := f.s.CountPendingWorkers(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, cap, n)
}

func TestDeleteRejectedWorkers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	old := f.worker(api.SandboxFull, f.pubID)
	_, err := f.s.w.ExecContext(f.ctx, `INSERT INTO grants (principal, role, scope, granted_by, granted_at)
		VALUES ('user:1', 'worker_admin', ?, 'user:1', 0)`, WorkerScope(old))
	require.NoError(t, err)
	_, err = f.s.SetWorkerState(f.ctx, old, api.WorkerRejected, "user:1")
	require.NoError(t, err)
	f.clk.Advance(31 * 24 * time.Hour)
	recent := f.worker(api.SandboxFull)
	_, err = f.s.SetWorkerState(f.ctx, recent, api.WorkerRejected, "user:1")
	require.NoError(t, err)
	active := f.worker(api.SandboxFull)

	n, err := f.s.DeleteRejectedWorkers(f.ctx, f.clk.Now().Add(-30*24*time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	_, err = f.s.WorkerByID(f.ctx, old)
	assert.ErrorIs(t, err, ErrNotFound)
	var grants int
	require.NoError(t, f.s.r.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM grants WHERE principal = ? OR scope = ?`,
		WorkerPrincipal(old), WorkerScope(old)).Scan(&grants))
	assert.Zero(t, grants, "grants held by or scoped to the worker go too")
	for _, id := range []string{recent, active} {
		_, err = f.s.WorkerByID(f.ctx, id)
		assert.NoError(t, err)
	}
}

// TestExtendLiveLeasesNeverShortens: the restart grace only ever lengthens live leases;
// a lease already expiring later keeps its expiry and ended leases are not
// touched.
func TestExtendLiveLeasesNeverShortens(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	f.job(f.pubID, "cli", api.WeightMedium, 3, 10)
	short := f.claim(w)
	long, err := f.s.Claim(f.ctx, w, f.eligibility(w), time.Hour)
	require.NoError(t, err)
	ended := f.claim(w)
	f.complete(ended)
	endedBefore, err := f.s.Lease(f.ctx, ended.ID, w)
	require.NoError(t, err)

	f.clk.Advance(time.Minute)
	n, err := f.s.ExtendLiveLeases(f.ctx, ttl)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n, "both live leases are counted")

	get := func(id string) *Lease {
		l, err := f.s.Lease(f.ctx, id, w)
		require.NoError(t, err)
		return l
	}
	assert.Equal(t, f.clk.Now().Add(ttl), get(short.ID).ExpiresAt, "extended to now+by")
	assert.Equal(t, long.ExpiresAt, get(long.ID).ExpiresAt, "a later expiry is never shortened")
	e := get(ended.ID)
	assert.Equal(t, endedBefore.ExpiresAt, e.ExpiresAt, "ended leases keep their expiry")
	assert.False(t, e.Live)
}
