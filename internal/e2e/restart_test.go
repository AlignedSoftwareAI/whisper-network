package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/e2e/e2etest"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

// TestServerRestartWithLiveLease (scenario 9): the server goes away for
// longer than a lease lives while a worker is mid-task. The worker keeps
// retrying its heartbeats and its result upload; the restarted server
// extends live leases by one TTL before its reaper runs, so the worker's
// result is accepted on the original lease.
func TestServerRestartWithLiveLease(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t, e2etest.FastLeases)
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	w := e.StartWorker("survivor", e2etest.ChildEnv(workertest.WhisperMode(workertest.SleepMode("2s"))))
	e.Approve(w.ID(), "public")
	leased := e.WaitEvent(obs.EventTaskLeased, func(m map[string]any) bool { return m[obs.FieldJobID] == job.ID })

	// Once a heartbeat arrived, the input is downloaded and the child runs.
	e.Eventually(func() bool { return len(e.Requests("/lease/{id}/heartbeat", "worker:"+w.ID())) > 0 },
		"no heartbeat")
	e.Stop()
	down := time.Now()
	w.WaitLog("", func(m map[string]any) bool { return m["message"] == "result upload failed; retrying" })
	// Stay down past the lease's expiry (3 × 1 s heartbeat interval from
	// the last heartbeat, which was before the stop).
	time.Sleep(3500*time.Millisecond - time.Since(down))
	e.Restart()

	done := e.WaitJob(job.ID, api.JobDone)
	assert.Equal(t, 1, done.Tasks.Done)
	ls := leasedFor(t, e, job.ID)
	require.Len(t, ls, 1, "the task was never re-leased")
	assert.Equal(t, leased[obs.FieldLeaseID], ls[0][obs.FieldLeaseID])
	assert.Empty(t, e.Logs.Events(t, obs.EventLeaseExpired), "restart grace kept the lease alive")
	w.WaitLog(obs.EventTaskDone, nil)
	assert.Contains(t, string(e.Logs.Bytes()), "extended live leases for restart grace")
}
