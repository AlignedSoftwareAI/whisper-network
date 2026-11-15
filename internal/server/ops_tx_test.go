package server

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // registers the "sqlite" driver for breakAudit

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/store"
)

// breakAudit makes every audit insert fail (an SQLite trigger added through
// a second connection) until the returned function is called.
func (h *harness) breakAudit() (restore func()) {
	h.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(h.dir, "wn.db")+"?_pragma=busy_timeout(5000)")
	require.NoError(h.t, err)
	_, err = db.ExecContext(h.ctx, `CREATE TRIGGER audit_broken BEFORE INSERT ON audit
		BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`)
	require.NoError(h.t, err)
	return func() {
		_, err := db.ExecContext(h.ctx, `DROP TRIGGER audit_broken`)
		require.NoError(h.t, err)
		require.NoError(h.t, db.Close())
	}
}

func (h *harness) auditCount() int {
	h.t.Helper()
	es, err := h.st.QueryAudit(h.ctx, store.AuditFilter{Limit: 1000})
	require.NoError(h.t, err)
	return len(es)
}

// auditFailed asserts that err is the injected audit failure, not some
// other refusal that would make the no-partial-state checks vacuous.
func auditFailed(t *testing.T, err error) {
	t.Helper()
	assert.ErrorContains(t, err, "audit unavailable")
}

// TestOpsAtomicWithAudit: when the audit append fails, every Ops mutation
// fails and leaves no partial state.
func TestOpsAtomicWithAudit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ops, a := h.srv.Ops(), CLIActor()
	pendingID, _ := h.worker(api.WorkerFacts{})
	activeID, _ := h.activeWorker(api.WorkerFacts{}, "public")
	victimID, owner := h.newClient(submitPublic)
	_, err := ops.CreateUser(h.ctx, a, api.CreateUserRequest{Username: "alice", Password: "alice-password-1"})
	require.NoError(t, err)
	aliceBefore, err := h.st.UserByUsername(h.ctx, "alice")
	require.NoError(t, err)
	job := h.submit(owner, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	pub, err := h.st.QueueByName(h.ctx, "public")
	require.NoError(t, err)

	audits := h.auditCount()
	restore := h.breakAudit()

	auditFailed(t, ops.ApproveWorker(h.ctx, a, pendingID, []string{"public"}))
	auditFailed(t, ops.Grant(h.ctx, a, "client:"+victimID, viewPublic))
	auditFailed(t, ops.Revoke(h.ctx, a, "client:"+victimID, submitPublic))
	auditFailed(t, ops.UpdateGrants(h.ctx, a, "client:"+victimID, []api.Grant{viewPublic}, []api.Grant{submitPublic}))
	auditFailed(t, ops.SetWorkerState(h.ctx, a, activeID, api.WorkerPaused))
	auditFailed(t, ops.RevokeClient(h.ctx, a, victimID))
	_, err = ops.CreateClient(h.ctx, a, "ghost", []api.Grant{submitPublic})
	auditFailed(t, err)
	_, err = ops.CreateUser(h.ctx, a, api.CreateUserRequest{Username: "ghost", Grants: []api.Grant{viewPublic}})
	auditFailed(t, err)
	_, err = ops.ResetPassword(h.ctx, a, "alice", "")
	auditFailed(t, err)
	auditFailed(t, ops.ChangePassword(h.ctx, aliceBefore, aliceBefore.PasswordHash, "a-new-password-2"))
	auditFailed(t, ops.DisableUser(h.ctx, a, "alice"))
	_, err = ops.CreateQueue(h.ctx, a, api.Queue{Name: "ghost"})
	auditFailed(t, err)
	desc := "changed"
	_, err = ops.PatchQueue(h.ctx, a, pub, api.QueuePatch{Description: &desc})
	auditFailed(t, err)
	auditFailed(t, ops.RenameQueue(h.ctx, a, pub, "renamed"))
	_, err = ops.DeleteJob(h.ctx, a, job.ID)
	auditFailed(t, err)
	_, err = ops.RegisterWorker(h.ctx, "some-hash", api.WorkerFacts{Hostname: "ghost"}, store.PendingLimits{})
	auditFailed(t, err)

	restore()
	assert.Equal(t, audits, h.auditCount(), "no audit entries were written")

	pw, err := h.st.WorkerByID(h.ctx, pendingID)
	require.NoError(t, err)
	assert.Equal(t, api.WorkerPending, pw.State)
	assert.Empty(t, h.wireGrants("worker:"+pendingID))
	aw, err := h.st.WorkerByID(h.ctx, activeID)
	require.NoError(t, err)
	assert.Equal(t, api.WorkerActive, aw.State)
	assert.Equal(t, []api.Grant{submitPublic}, h.wireGrants("client:"+victimID))
	vc, err := h.st.ClientByID(h.ctx, victimID)
	require.NoError(t, err)
	assert.Nil(t, vc.RevokedAt)
	cs, err := h.st.ListClients(h.ctx)
	require.NoError(t, err)
	for _, c := range cs {
		assert.NotEqual(t, "ghost", c.Name)
	}
	_, err = h.st.UserByUsername(h.ctx, "ghost")
	assert.ErrorIs(t, err, store.ErrNotFound)
	alice, err := h.st.UserByUsername(h.ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, aliceBefore.PasswordHash, alice.PasswordHash)
	assert.Nil(t, alice.DisabledAt)
	_, err = h.st.QueueByName(h.ctx, "ghost")
	assert.ErrorIs(t, err, store.ErrNotFound)
	q, err := h.st.QueueByName(h.ctx, "public")
	require.NoError(t, err)
	assert.Equal(t, pub.Description, q.Description)
	_, err = h.st.Job(h.ctx, job.ID)
	assert.NoError(t, err)
}

// TestApproveUnknownQueueChangesNothing: approving for a queue that does
// not exist fails before anything is written: the worker stays pending with
// no grants and nothing is audited.
func TestApproveUnknownQueueChangesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id, _ := h.worker(api.WorkerFacts{})
	audits := h.auditCount()
	resp, b := h.call("POST", "/admin/workers/"+id+"/approve", h.admin,
		api.ApproveWorkerRequest{Queues: []string{"public", "nope"}})
	h.expectProblem(resp, b, http.StatusBadRequest, api.ProblemBadRequest)
	w, err := h.st.WorkerByID(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, api.WorkerPending, w.State)
	assert.Empty(t, h.wireGrants("worker:"+id))
	assert.Equal(t, audits, h.auditCount())
}

// TestResetPasswordMustChange: a generated reset password must be changed
// at next login; an explicitly chosen one need not be.
func TestResetPasswordMustChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ops := h.srv.Ops()
	_, err := ops.CreateUser(h.ctx, CLIActor(), api.CreateUserRequest{Username: "alice", Password: "alice-password-1"})
	require.NoError(t, err)
	pw, err := ops.ResetPassword(h.ctx, CLIActor(), "alice", "")
	require.NoError(t, err)
	u, err := h.st.UserByUsername(h.ctx, "alice")
	require.NoError(t, err)
	assert.True(t, u.MustChangePassword)
	_, err = h.srv.Login(h.ctx, "alice", pw)
	require.NoError(t, err)

	require.NoError(t, ops.ChangePassword(h.ctx, u, u.PasswordHash, "alice-password-2"))
	u, err = h.st.UserByUsername(h.ctx, "alice")
	require.NoError(t, err)
	assert.False(t, u.MustChangePassword)

	_, err = ops.ResetPassword(h.ctx, CLIActor(), "alice", "chosen-password-3")
	require.NoError(t, err)
	u, err = h.st.UserByUsername(h.ctx, "alice")
	require.NoError(t, err)
	assert.False(t, u.MustChangePassword)
}

// TestPruneAndExpireAudited: housekeeping deletions are audited by the
// system, one entry per pruned worker and one per non-empty expiry sweep.
func TestPruneAndExpireAudited(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ops := h.srv.Ops()
	id, _ := h.worker(api.WorkerFacts{})
	h.clk.Advance(48 * time.Hour)
	n, err := ops.PrunePendingWorkers(h.ctx, h.clk.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	es, err := h.st.QueryAudit(h.ctx, store.AuditFilter{ActionPrefix: "worker.prune"})
	require.NoError(t, err)
	require.Len(t, es, 1)
	assert.Equal(t, "worker:"+id, es[0].Target)
	assert.Equal(t, store.SystemActor, es[0].Actor)

	n, err = ops.DeleteExpiredSessions(h.ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
	es, err = h.st.QueryAudit(h.ctx, store.AuditFilter{ActionPrefix: "session.expire"})
	require.NoError(t, err)
	assert.Empty(t, es, "an empty sweep is not audited")
}
