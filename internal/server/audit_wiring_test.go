package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/store"
)

// auditSince returns the audit entries written after mark, oldest first.
func (h *harness) auditSince(mark int64) []store.AuditEntry {
	h.t.Helper()
	es, err := h.st.QueryAudit(h.ctx, store.AuditFilter{Limit: 1000})
	require.NoError(h.t, err)
	var out []store.AuditEntry
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].ID > mark {
			out = append(out, es[i])
		}
	}
	return out
}

// auditStep runs do and asserts the audit entries it wrote, given as
// "action actor target" lines in any order; a field of "*" matches
// anything.
func (h *harness) auditStep(want []string, do func()) []store.AuditEntry {
	h.t.Helper()
	mark := h.auditMark()
	do()
	es := h.auditSince(mark)
	got := make([]string, len(es))
	for i, e := range es {
		got[i] = e.Action + " " + e.Actor + " " + e.Target
	}
	used := make([]bool, len(got))
	ok := len(want) == len(got)
	for _, w := range want {
		found := false
		for i, g := range got {
			if !used[i] && auditLineMatches(w, g) {
				used[i], found = true, true
				break
			}
		}
		ok = ok && found
	}
	assert.True(h.t, ok, "audit entries: want %q, got %q", want, got)
	return es
}

func auditLineMatches(want, got string) bool {
	wf, gf := strings.SplitN(want, " ", 3), strings.SplitN(got, " ", 3)
	if len(wf) != len(gf) {
		return false
	}
	for i := range wf {
		if wf[i] != "*" && wf[i] != gf[i] {
			return false
		}
	}
	return true
}

// TestAuditWiredRequestPaths drives the audited changes that happen on
// request paths outside the admin API (uploads, deletions, worker
// self-service, sign-in, password change) through the same entry points
// the server and UI use, and checks each writes exactly the expected audit
// rows and no secrets.
func TestAuditWiredRequestPaths(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	adminID, admin := h.newClient(api.Grant{Role: api.RoleAdmin, Scope: "*"})
	ownerID, owner := h.newClient(submitPublic)
	ownerName, adminName := "client:"+ownerID, "client:"+adminID
	secrets := []string{admin, owner}

	// Upload: canonical WAV takes the fast path, which also purges the
	// original; anything else is only created. A replayed idempotency key
	// writes nothing.
	var fast api.Job
	h.auditStep(nil, func() {}) // a no-op writes nothing
	h.auditStep([]string{"job.create " + ownerName + " *", "job.purge_media system *"}, func() {
		fast = h.submit(owner, "public", api.JobParams{Model: "small.en", Prompt: "upload-secret-prompt"},
			canonicalWAV(speech(1, media.SampleRate)))
	})
	secrets = append(secrets, "upload-secret-prompt")
	h.auditStep([]string{"job.create " + ownerName + " *"}, func() {
		resp, b := h.upload(owner, "public", api.JobParams{Model: "small.en"}, []byte("opaque video"), "key-1")
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(b))
	})
	h.auditStep(nil, func() {
		resp, b := h.upload(owner, "public", api.JobParams{Model: "small.en"}, []byte("opaque video"), "key-1")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	})
	for _, e := range h.auditSince(0) {
		if e.Action == "job.create" || e.Action == "job.purge_media" {
			assert.Regexp(t, `^job:`, e.Target)
		}
	}

	// Deletion by an admin over the API, and by a UI user through the seam.
	h.auditStep([]string{"job.delete " + adminName + " job:" + fast.ID}, func() {
		h.expect("DELETE", "/job/"+fast.ID, admin, nil, http.StatusNoContent, nil)
	})
	viaUI := h.submit(owner, "public", api.JobParams{Model: "small.en"}, []byte("more opaque video"))
	adminUser, err := h.srv.Login(h.ctx, "admin", "admin-password")
	require.NoError(t, err)
	p, err := h.srv.PrincipalForUser(h.ctx, adminUser)
	require.NoError(t, err)
	h.auditStep([]string{"job.delete user:admin job:" + viaUI.ID}, func() {
		require.NoError(t, h.srv.DeleteJob(h.ctx, p, viaUI.ID))
	})
	h.auditStep(nil, func() {
		resp, b := h.call("DELETE", "/job/"+viaUI.ID, admin, nil)
		h.expectProblem(resp, b, http.StatusNotFound, api.ProblemNotFound)
	})

	// Worker self-registration and facts updates.
	var wid, wtok string
	es := h.auditStep([]string{"worker.register * *"}, func() { wid, wtok = h.worker(api.WorkerFacts{}) })
	wname := "worker:" + wid
	assert.Equal(t, wname, es[0].Actor)
	assert.Equal(t, wname, es[0].Target)
	secrets = append(secrets, wtok)
	h.expect("POST", "/admin/workers/"+wid+"/approve", admin, api.ApproveWorkerRequest{Queues: []string{"public"}},
		http.StatusNoContent, nil)
	h.auditStep([]string{"worker.update_facts " + wname + " " + wname}, func() {
		h.expect("PATCH", "/worker/"+wid, wtok, api.WorkerFacts{Hostname: "renamed", SandboxLevel: api.SandboxFull,
			Slots: 2}, http.StatusOK, nil)
	})
	h.auditStep(nil, func() {
		resp, b := h.call("PATCH", "/worker/"+wid, wtok, api.WorkerFacts{})
		h.expectProblem(resp, b, http.StatusBadRequest, api.ProblemBadRequest)
	})

	// Sign-in, rotation and sign-out through the UI seam.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/ui/login", nil)
	r.RemoteAddr = "192.0.2.7:1234"
	es = h.auditStep([]string{"auth.login user:admin user:admin"}, func() {
		_, err := h.srv.StartSession(rec, r, adminUser)
		require.NoError(t, err)
	})
	assert.JSONEq(t, `{"ip":"192.0.2.7"}`, es[0].Detail)
	first := rec.Result().Cookies()[0]
	r = httptest.NewRequest("POST", "/ui/login", nil)
	r.AddCookie(first)
	rec = httptest.NewRecorder()
	h.auditStep([]string{"auth.login user:admin user:admin"}, func() {
		_, err := h.srv.StartSession(rec, r, adminUser)
		require.NoError(t, err)
	})
	r = httptest.NewRequest("GET", "/ui/", nil)
	r.AddCookie(first)
	sess, _, err := h.srv.SessionFromRequest(r)
	require.NoError(t, err)
	assert.Nil(t, sess, "the session the login replaced is gone")
	second := rec.Result().Cookies()[0]
	secrets = append(secrets, first.Value, second.Value)
	r = httptest.NewRequest("POST", "/ui/logout", nil)
	r.AddCookie(second)
	h.auditStep([]string{"auth.logout user:admin user:admin"}, func() {
		require.NoError(t, h.srv.EndSession(httptest.NewRecorder(), r))
	})
	h.auditStep(nil, func() { require.NoError(t, h.srv.EndSession(httptest.NewRecorder(), r)) })

	// Password change through the seam the UI uses.
	h.auditStep([]string{"user.change_password user:admin user:admin"}, func() {
		require.NoError(t, h.srv.ChangePassword(h.ctx, adminUser, adminUser.PasswordHash, "admin-password-2"))
	})
	secrets = append(secrets, "admin-password", "admin-password-2")

	assertAuditClean(t, h, secrets)
}

// TestAuditReaper: the reaper's purges, deletions, prunes and session
// expiry are each audited by the system, once per change.
func TestAuditReaper(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	done := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	resp, b := h.putResult(l, wtok, result(l, "kept"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	pendingID, _ := h.worker(api.WorkerFacts{Hostname: "abandoned"})
	rejectedID, _ := h.worker(api.WorkerFacts{Hostname: "unwanted"})
	h.expect("POST", "/admin/workers/"+rejectedID+"/reject", h.admin, nil, http.StatusNoContent, nil)
	u, err := h.srv.Login(h.ctx, "admin", "admin-password")
	require.NoError(t, err)
	_, err = h.srv.StartSession(httptest.NewRecorder(), httptest.NewRequest("POST", "/ui/login", nil), u)
	require.NoError(t, err)

	h.clk.Advance(h.srv.cfg.AudioRetention + time.Second)
	es := h.auditStep([]string{"job.purge_media system job:" + done.ID, "session.expire system "},
		func() { h.srv.reapOnce(h.ctx) })
	for _, e := range es {
		if e.Action == "job.purge_media" {
			assert.JSONEq(t, `{"what":"canonical"}`, e.Detail)
		}
	}
	h.auditStep(nil, func() { h.srv.reapOnce(h.ctx) })

	h.clk.Advance(h.srv.cfg.JobRetention)
	es = h.auditStep([]string{"job.delete system job:" + done.ID, "worker.prune system worker:" + pendingID,
		"worker.prune system worker:" + rejectedID}, func() { h.srv.reapOnce(h.ctx) })
	for _, e := range es {
		switch e.Target {
		case "worker:" + pendingID:
			assert.JSONEq(t, `{"state":"pending"}`, e.Detail)
		case "worker:" + rejectedID:
			assert.JSONEq(t, `{"state":"rejected"}`, e.Detail)
		}
	}
	_, err = h.st.WorkerByID(h.ctx, wid)
	assert.NoError(t, err, "active workers stay")
	h.auditStep(nil, func() { h.srv.reapOnce(h.ctx) })
	assertAuditClean(t, h, []string{wtok, client, "admin-password"})
}

// TestRegisterCapNotAudited: a registration refused by the pending cap
// changes nothing and writes no audit row; the cap and the audit share the
// registration's transaction.
func TestRegisterCapNotAudited(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) { c.MaxPendingWorkers = 1 })
	h.auditStep([]string{"worker.register * *"}, func() { h.worker(api.WorkerFacts{}) })
	h.auditStep(nil, func() {
		resp, b := h.call("POST", "/worker", "", api.WorkerFacts{Hostname: "second", SandboxLevel: api.SandboxFull})
		h.expectProblem(resp, b, http.StatusServiceUnavailable, api.ProblemRateLimited)
	})
	n, err := h.st.CountPendingWorkers(h.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// TestQueueCapAboveServerCap: no caller, admin included, may set a queue's
// upload cap above the server's --max-upload-bytes.
func TestQueueCapAboveServerCap(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) { c.MaxUploadBytes = 1 << 20 })
	over, at := int64(1<<20+1), int64(1<<20)
	h.auditStep(nil, func() {
		resp, b := h.call("PATCH", "/queue/public", h.admin, api.QueuePatch{MaxUploadBytes: &over})
		p := h.expectProblem(resp, b, http.StatusBadRequest, api.ProblemBadRequest)
		assert.Contains(t, p.Detail, "--max-upload-bytes")
		resp, b = h.call("POST", "/admin/queues", h.admin, api.Queue{Name: "big", MaxUploadBytes: over})
		h.expectProblem(resp, b, http.StatusBadRequest, api.ProblemBadRequest)
	})
	h.expect("PATCH", "/queue/public", h.admin, api.QueuePatch{MaxUploadBytes: &at}, http.StatusOK, nil)
	q, err := h.st.QueueByName(h.ctx, "public")
	require.NoError(t, err)
	assert.Equal(t, at, q.MaxUploadBytes)
}

// TestOpsPasswordHashingHoldsSlot: Ops hashes under the server's password
// gate, so admin user creation and resets are refused with 503 when it is
// saturated.
func TestOpsPasswordHashingHoldsSlot(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) {
		c.BusyWait = 20 * time.Millisecond
		c.PasswordMemoryBudget = 1 // two slots
	})
	r1, err := h.srv.PasswordSlot(h.ctx)
	require.NoError(t, err)
	r2, err := h.srv.PasswordSlot(h.ctx)
	require.NoError(t, err)
	busy := func(err error) {
		t.Helper()
		secs, ok := Busy(err)
		assert.True(t, ok, "%v", err)
		assert.Positive(t, secs)
	}
	ops := h.srv.Ops()
	h.auditStep(nil, func() {
		_, err := ops.CreateUser(h.ctx, CLIActor(), api.CreateUserRequest{Username: "dave"})
		busy(err)
		_, err = ops.ResetPassword(h.ctx, CLIActor(), "admin", "")
		busy(err)
		u, err := h.st.UserByUsername(h.ctx, "admin")
		require.NoError(t, err)
		busy(ops.ChangePassword(h.ctx, u, u.PasswordHash, "admin-password-3"))
	})
	r1()
	r2()
	_, ok := Busy(badRequest("x"))
	assert.False(t, ok)
	_, err = ops.CreateUser(h.ctx, CLIActor(), api.CreateUserRequest{Username: "dave"})
	require.NoError(t, err)
}

// TestPruneAtomicWithAudit: when the audit append fails, pruning pending
// and rejected workers fails and deletes nothing.
func TestPruneAtomicWithAudit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	pendingID, _ := h.worker(api.WorkerFacts{Hostname: "abandoned"})
	rejectedID, _ := h.worker(api.WorkerFacts{Hostname: "unwanted"})
	h.expect("POST", "/admin/workers/"+rejectedID+"/reject", h.admin, nil, http.StatusNoContent, nil)
	h.clk.Advance(h.srv.cfg.RejectedWorkerRetention + time.Second)
	cutoff := h.clk.Now().Add(-time.Hour)

	restore := h.breakAudit()
	_, err := h.srv.Ops().PrunePendingWorkers(h.ctx, cutoff)
	auditFailed(t, err)
	_, err = h.srv.Ops().PruneRejectedWorkers(h.ctx, cutoff)
	auditFailed(t, err)
	restore()
	for _, id := range []string{pendingID, rejectedID} {
		_, err := h.st.WorkerByID(h.ctx, id)
		assert.NoError(t, err, "worker %s survives a failed prune", id)
	}
}
