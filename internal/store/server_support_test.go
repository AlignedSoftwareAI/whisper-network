package store

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

func TestPing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	require.NoError(t, f.s.Ping(f.ctx))
}

func TestWorkerTokenRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w, err := f.s.RegisterWorker(f.ctx, "wt", api.WorkerFacts{Hostname: "h"})
	require.NoError(t, err)

	rejected, err := f.s.WorkerTokenRejected(f.ctx, "wt")
	require.NoError(t, err)
	assert.False(t, rejected, "pending worker")
	rejected, err = f.s.WorkerTokenRejected(f.ctx, "unknown")
	require.NoError(t, err)
	assert.False(t, rejected, "unknown token")

	_, err = f.s.SetWorkerState(f.ctx, w.ID, api.WorkerRejected, "user:1")
	require.NoError(t, err)
	rejected, err = f.s.WorkerTokenRejected(f.ctx, "wt")
	require.NoError(t, err)
	assert.True(t, rejected)
}

func TestJobsAwaitingAssembly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c1", api.WeightMedium, 2, 60)
	f.prepJob(f.pubID, "c1") // preparing jobs have no transcribe tasks yet

	ids, err := f.s.JobsAwaitingAssembly(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, ids)

	assert.False(t, f.complete(f.claim(w)))
	ids, err = f.s.JobsAwaitingAssembly(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, ids, "one task still pending")

	assert.True(t, f.complete(f.claim(w)))
	ids, err = f.s.JobsAwaitingAssembly(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{j.ID}, ids)

	_, err = f.s.MarkJobDone(f.ctx, j.ID)
	require.NoError(t, err)
	ids, err = f.s.JobsAwaitingAssembly(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, ids, "done jobs are assembled")
}

// TestConcurrentOpenOfFreshDatabase: the server and the offline admin
// command may open a new database at the same moment; migrations must be
// applied exactly once, and neither open may fail. While one connection
// switches the new file to WAL mode, the other's BEGIN gets SQLITE_BUSY
// without busy_timeout being consulted, so Open retries it; before that
// about half of the rounds failed. Several rounds make the race likely.
func TestConcurrentOpenOfFreshDatabase(t *testing.T) {
	t.Parallel()
	for round := range 20 {
		path := filepath.Join(t.TempDir(), "fresh.db")
		const n = 4
		errs := make(chan error, n)
		var wg sync.WaitGroup
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s, err := Open(t.Context(), Config{Path: path})
				if err == nil {
					err = s.Close()
				}
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err, "round %d", round)
		}
	}
}
