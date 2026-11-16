package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
)

func processQueues(t *testing.T, e *env, workerID string) []string {
	t.Helper()
	gs, err := e.srv.Ops().WireGrants(e.ctx, store.WorkerPrincipal(workerID))
	require.NoError(t, err)
	var out []string
	for _, g := range gs {
		if g.Role == api.RoleProcess {
			out = append(out, strings.TrimPrefix(g.Scope, "queue:"))
		}
	}
	return out
}

func workerState(t *testing.T, e *env, id string) api.WorkerState {
	t.Helper()
	wk, err := e.st.WorkerByID(e.ctx, id)
	require.NoError(t, err)
	return wk.State
}

// TestApproveWorker: approval needs worker_admin over the worker and
// queue_admin on every chosen queue (DESIGN.md §5.2).
func TestApproveWorker(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.queue("other")
	wid, _ := e.worker("box", "a box")
	e.user("lifecycle", api.Grant{Role: api.RoleWorkerAdmin, Scope: "*"})
	e.user("both", api.Grant{Role: api.RoleWorkerAdmin, Scope: "*"},
		api.Grant{Role: api.RoleQueueAdmin, Scope: auth.QueueScope("public")})

	// worker_admin alone cannot hand out queue access...
	b := e.login("lifecycle")
	_, body := b.get("/ui/workers/" + wid)
	assert.Contains(t, body, "Approve")
	assert.NotContains(t, body, `name="queue"`, "no queue to offer")
	resp, _ := b.post("/ui/workers/"+wid+"/approve", url.Values{"queue": {"public"}})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, api.WorkerPending, workerState(t, e, wid))

	// ...and queue_admin on public does not cover other.
	b = e.login("both")
	_, body = b.get("/ui/workers/" + wid)
	assert.Contains(t, body, `value="public"`)
	assert.NotContains(t, body, `value="other"`)
	resp, _ = b.post("/ui/workers/"+wid+"/approve", url.Values{"queue": {"public", "other"}})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, api.WorkerPending, workerState(t, e, wid))

	// Approving through htmx navigates back to the worker's page.
	resp, body = b.do("POST", "/ui/workers/"+wid+"/approve", url.Values{"queue": {"public"}, "csrf": {b.csrf}},
		map[string]string{"HX-Request": "true"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	assert.Equal(t, "/ui/workers/"+wid, resp.Header.Get("HX-Redirect"))
	assert.Equal(t, api.WorkerActive, workerState(t, e, wid))
	assert.Equal(t, []string{"public"}, processQueues(t, e, wid))

	audit, err := e.st.ListAudit(e.ctx, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, "worker.approve", audit[0].Action)
	assert.Equal(t, "user:both", audit[0].Actor)
}

func TestWorkerLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	wid, _ := e.worker("box", "a box")
	b := e.login("root")

	resp, _ := b.post("/ui/workers/"+wid+"/resume", nil)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "a pending worker is approved, not resumed")
	resp, _ = b.post("/ui/workers/"+wid+"/approve", url.Values{"queue": {"public"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/workers/"+wid, resp.Header.Get("Location"))

	resp, _ = b.post("/ui/workers/"+wid+"/pause", nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, api.WorkerPaused, workerState(t, e, wid))
	_, body := b.get("/ui/workers")
	assert.Contains(t, body, "Resume")
	assert.NotContains(t, body, ">Pause<")

	resp, _ = b.post("/ui/workers/"+wid+"/resume", nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, api.WorkerActive, workerState(t, e, wid))

	// Queue access can be extended and revoked from the worker's page.
	e.queue("other")
	_, body = b.get("/ui/workers/" + wid)
	assert.Contains(t, body, `<option value="queue:other">`)
	assert.Contains(t, body, `name="back" value="/ui/workers/`+wid+`"`)
	resp, _ = b.post("/ui/grants", url.Values{"principal": {"worker:" + wid}, "role": {"process"},
		"scope": {"queue:other"}, "back": {"/ui/workers/" + wid}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/workers/"+wid, resp.Header.Get("Location"))
	assert.ElementsMatch(t, []string{"public", "other"}, processQueues(t, e, wid))
	resp, _ = b.post("/ui/grants/revoke", url.Values{"principal": {"worker:" + wid}, "role": {"process"},
		"scope": {"queue:public"}, "back": {"https://evil.example/"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/", resp.Header.Get("Location"), "back is checked like next")
	assert.Equal(t, []string{"other"}, processQueues(t, e, wid))

	resp, _ = b.post("/ui/workers/"+wid+"/reject", nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, api.WorkerRejected, workerState(t, e, wid))
	_, body = b.get("/ui/workers/" + wid)
	assert.NotContains(t, body, ">Reject<")

	resp, _ = b.post("/ui/workers/"+wid+"/explode", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp, _ = b.post("/ui/workers/no-such-worker/pause", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestQueueWeightAdminOnly: weight is a fleet-wide decision; queue admins
// edit only description and upload cap (DESIGN.md §5.2).
func TestQueueWeightAdminOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.queue("other")
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	e.user("qa", api.Grant{Role: api.RoleQueueAdmin, Scope: auth.QueueScope("other")})
	weight := func() int {
		q, err := e.st.QueueByName(e.ctx, "other")
		require.NoError(t, err)
		return q.Weight
	}

	qa := e.login("qa")
	resp, _ := qa.post("/ui/queues/other", url.Values{"description": {"meetings"}, "weight": {"900"}})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, 100, weight())
	resp, _ = qa.post("/ui/queues/other", url.Values{"description": {"meetings"}, "max_upload": {"500MiB"}, "weight": {"100"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, "an unchanged weight is not a change")
	q, err := e.st.QueueByName(e.ctx, "other")
	require.NoError(t, err)
	assert.Equal(t, "meetings", q.Description)
	assert.Equal(t, int64(500<<20), q.MaxUploadBytes)

	root := e.login("root")
	resp, _ = root.post("/ui/queues/other", url.Values{"weight": {"300"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, 300, weight())
	resp, body := root.post("/ui/queues/other", url.Values{"weight": {"0"}})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	resp, _ = root.post("/ui/queues/other", url.Values{"weight": {"lots"}})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	audit, err := e.st.ListAudit(e.ctx, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, "queue.patch", audit[0].Action)
	assert.Equal(t, "user:root", audit[0].Actor)
	assert.Contains(t, audit[0].Detail, "300")
}

func TestCreateQueue(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	b := e.login("root")
	resp, _ := b.post("/ui/queues", url.Values{"name": {"meetings"}, "description": {"council"}, "weight": {"300"},
		"max_upload": {"1GiB"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/queues/meetings", resp.Header.Get("Location"))
	q, err := e.st.QueueByName(e.ctx, "meetings")
	require.NoError(t, err)
	assert.Equal(t, 300, q.Weight)
	assert.Equal(t, int64(1<<30), q.MaxUploadBytes)

	resp, _ = b.post("/ui/queues", url.Values{"name": {"meetings"}})
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp, _ = b.post("/ui/queues", url.Values{"name": {"Bad Name"}})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

var (
	tokenRE  = regexp.MustCompile(api.ClientTokenPrefix + `[A-Za-z0-9]+`)
	secretRE = regexp.MustCompile(`<code class="token">([^<]+)</code>`) // a one-time secret box
)

// TestClientTokenShownOnce: the token appears in the create response only,
// works on the API, and never again in the UI.
func TestClientTokenShownOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	b := e.login("root")
	_, body := b.get("/ui/clients")
	assert.Contains(t, body, `value="submit queue:public"`)

	resp, body := b.post("/ui/clients", url.Values{"name": {"transcriber"}, "grant": {"submit queue:public"}})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	token := tokenRE.FindString(body)
	require.NotEmpty(t, token)
	assert.Contains(t, body, "only this once")

	status, _ := e.api("GET", "/queue/public", token, nil)
	assert.Equal(t, http.StatusOK, status, "the minted token works")
	status, _ = e.api("GET", "/queue/other-queue", token, nil)
	assert.Equal(t, http.StatusNotFound, status)

	_, body = b.get("/ui/clients")
	assert.Contains(t, body, "transcriber")
	assert.NotContains(t, body, token)
	assert.Empty(t, tokenRE.FindString(body))

	// Revoke it.
	cs, err := e.st.ListClients(e.ctx)
	require.NoError(t, err)
	var id string
	for _, c := range cs {
		if c.Name == "transcriber" {
			id = c.ID
		}
	}
	resp, _ = b.post("/ui/clients/"+id+"/revoke", nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	status, _ = e.api("GET", "/queue/public", token, nil)
	assert.Equal(t, http.StatusUnauthorized, status)
}

// TestClientGrantsCannotEscalate: a client_admin hands out only grants it
// could give (auth.CanDelegateAtCreation).
func TestClientGrantsCannotEscalate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.queue("other")
	e.user("ca", api.Grant{Role: api.RoleClientAdmin, Scope: "*"},
		api.Grant{Role: api.RoleQueueAdmin, Scope: auth.QueueScope("other")})
	b := e.login("ca")
	_, body := b.get("/ui/clients")
	assert.Contains(t, body, `value="submit queue:other"`)
	assert.NotContains(t, body, `value="submit queue:public"`)
	assert.NotContains(t, body, `value="admin *"`)

	for _, g := range []string{"submit queue:public", "admin *", "process queue:other", "nonsense"} {
		resp, _ := b.post("/ui/clients", url.Values{"name": {"x"}, "grant": {g}})
		assert.Contains(t, []int{http.StatusForbidden, http.StatusBadRequest}, resp.StatusCode, g)
	}
	resp, _ := b.post("/ui/clients", url.Values{"name": {"ok"}, "grant": {"submit queue:other", "view_all queue:other"}})
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	cs, err := e.st.ListClients(e.ctx)
	require.NoError(t, err)
	var names []string
	for _, c := range cs {
		names = append(names, c.Name)
	}
	assert.NotContains(t, names, "x")
}

// TestCreateUserPasswordShownOnce: a generated password is shown once and
// must be changed at first sign-in.
func TestCreateUserPasswordShownOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	b := e.login("root")
	resp, body := b.post("/ui/users", url.Values{"username": {"newbie"}, "grant": {"view_all queue:public"}})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	m := secretRE.FindStringSubmatch(body)
	require.NotNil(t, m)
	password := m[1]

	_, body = b.get("/ui/users")
	assert.NotContains(t, body, password)
	assert.Contains(t, body, "must change password")

	nb := e.browser()
	resp, _ = nb.submitLogin("newbie", password, "")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.True(t, strings.HasPrefix(resp.Header.Get("Location"), "/ui/password"))

	resp, _ = b.post("/ui/users", url.Values{"username": {"weak"}, "password": {"short"}})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp, _ = b.post("/ui/users", url.Values{"username": {"Bad User"}})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestUserGrantsAndDisable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	e.user("bob")
	b := e.login("root")
	bob := e.login("bob")

	resp, _ := b.post("/ui/grants", url.Values{"principal": {"user:bob"}, "role": {"view_all"},
		"scope": {"queue:public"}, "back": {"/ui/users"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	_, body := bob.get("/ui/queues/public")
	assert.NotContains(t, body, "no access to this queue", "bob now sees public's jobs")
	resp, _ = b.post("/ui/grants", url.Values{"principal": {"user:bob"}, "role": {"process"},
		"scope": {"queue:public"}, "back": {"/ui/users"}})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "process is for workers")
	resp, _ = b.post("/ui/grants", url.Values{"principal": {"user:nobody"}, "role": {"submit"},
		"scope": {"queue:public"}})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, _ = b.post("/ui/users/root/disable", nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "no self-lockout")
	resp, _ = b.post("/ui/users/bob/disable", nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp, _ = bob.get("/ui/")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "bob's session ended")
	_, body = b.get("/ui/users")
	assert.Contains(t, body, "disabled")
}

// TestQueueGrants: a queue admin manages client and user access to its
// own queue from the queue page.
func TestQueueGrants(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.queue("other")
	e.user("qa", api.Grant{Role: api.RoleQueueAdmin, Scope: auth.QueueScope("other")})
	e.user("bob")
	b := e.login("qa")
	_, body := b.get("/ui/queues/other")
	assert.Contains(t, body, `qa <span class="muted">(user)</span>`)
	assert.Contains(t, body, "<option>submit</option>")
	assert.NotContains(t, body, "<option>queue_admin</option>", "queue admins cannot create other queue admins")
	assert.NotContains(t, body, "<option>process</option>", "workers get process through approval")

	resp, _ := b.post("/ui/grants", url.Values{"principal": {"user:bob"}, "role": {"submit"},
		"scope": {"queue:other"}, "back": {"/ui/queues/other"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/queues/other", resp.Header.Get("Location"))
	_, body = b.get("/ui/queues/other")
	assert.Contains(t, body, "user:bob")
}

func TestJobDetailAndDelete(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	e := f.env
	// A worker runs the job to completion so the transcript is offered.
	wid, wtok := e.worker("runner", "")
	require.NoError(t, e.srv.Ops().ApproveWorker(e.ctx, server.CLIActor(), wid, []string{"public"}))
	e.runJob(wid, wtok)

	b := f.login("root")
	resp, body := b.get("/ui/jobs/" + f.job.ID)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "job-done")
	assert.Contains(t, body, `href="/job/`+f.job.ID+`/transcript"`)
	assert.Contains(t, body, "transcribe")

	// The transcript link is the API endpoint, which accepts the session.
	resp, body = b.get("/job/" + f.job.ID + "/transcript")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "hello")

	// Deleting through htmx tells the page to navigate to the queue.
	resp, _ = b.do("POST", "/ui/jobs/"+f.job.ID+"/delete", url.Values{"csrf": {b.csrf}},
		map[string]string{"HX-Request": "true"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "/ui/queues/public", resp.Header.Get("HX-Redirect"))
	_, err := e.st.Job(e.ctx, f.job.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	resp, _ = b.get("/ui/jobs/" + f.job.ID)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	audit, err := e.st.ListAudit(e.ctx, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, "job.delete", audit[0].Action)
}

func TestAuditPaging(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	for i := range auditPageSize + 5 {
		e.queue(fmt.Sprintf("q%02d", i))
	}
	b := e.login("root")
	_, body := b.get("/ui/audit")
	assert.Equal(t, auditPageSize, strings.Count(body, "<tr><td>"))
	assert.Contains(t, body, `href="/ui/queues/q54"`, "newest first")
	m := regexp.MustCompile(`href="(/ui/audit\?before=\d+)"`).FindStringSubmatch(body)
	require.NotNil(t, m)
	resp, body := b.get(m[1])
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, `href="/ui/queues/q00"`)
	assert.NotContains(t, body, `href="/ui/queues/q54"`)
	resp, _ = b.get("/ui/audit?before=-1")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
