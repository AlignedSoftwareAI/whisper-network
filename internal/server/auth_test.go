package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/obs/obstest"
	"github.com/deweysasser/whisper-network/internal/store"
)

// TestAuthMatrix exercises every endpoint's authentication and
// authorization failures: no credentials, a malformed or unknown token, the
// wrong principal kind, missing grants, and invisible jobs (404, not 403).
func TestAuthMatrix(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.expect("POST", "/admin/queues", h.admin, api.Queue{Name: "private"}, http.StatusCreated, nil)
	owner := h.client(submitOn("public"))
	stranger := h.client(submitOn("public"))
	viewer := h.client(api.Grant{Role: api.RoleViewAll, Scope: "queue:public"})
	reader := h.client(api.Grant{Role: api.RoleReadAll, Scope: "queue:public"})
	privateOnly := h.client(submitOn("private"))
	noGrants := h.client()
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(owner, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	require.NotNil(t, l)
	_, otherWTok := h.activeWorker(api.WorkerFacts{}, "public")
	const badToken = "wnc_0000000000000000000000000000000000000000000"

	jobPath := "/job/" + job.ID
	cases := []struct {
		name, method, path, token string
		status                    int
		slug                      string
	}{
		// Unauthenticated and bad tokens.
		{"no auth job", "GET", jobPath, "", 401, api.ProblemUnauthorized},
		{"no auth queue", "GET", "/queue/public", "", 401, api.ProblemUnauthorized},
		{"no auth claim", "POST", "/worker/" + wid + "/leases", "", 401, api.ProblemUnauthorized},
		{"no auth heartbeat", "POST", "/lease/" + l.ID + "/heartbeat", "", 401, api.ProblemUnauthorized},
		{"no auth model", "GET", "/models/small.en", "", 401, api.ProblemUnauthorized},
		{"no auth admin", "GET", "/admin/workers", "", 401, api.ProblemUnauthorized},
		{"malformed token", "GET", jobPath, "garbage", 401, api.ProblemUnauthorized},
		{"unknown token", "GET", jobPath, badToken, 401, api.ProblemUnauthorized},
		// Wrong principal kind.
		{"worker reads job", "GET", jobPath, wtok, 403, api.ProblemForbidden},
		{"worker lists queue", "GET", "/queue/public", wtok, 403, api.ProblemForbidden},
		{"worker creates client", "POST", "/client", wtok, 403, api.ProblemForbidden},
		{"worker admin", "GET", "/admin/workers", wtok, 403, api.ProblemForbidden},
		{"client claims", "POST", "/worker/" + wid + "/leases", owner, 403, api.ProblemForbidden},
		{"client heartbeat", "POST", "/lease/" + l.ID + "/heartbeat", owner, 403, api.ProblemForbidden},
		{"client input", "GET", l.InputURL, owner, 403, api.ProblemForbidden},
		{"client model", "GET", "/models/small.en", h.admin, 403, api.ProblemForbidden},
		// Invisible jobs are 404, visible but forbidden 403.
		{"stranger reads job", "GET", jobPath, stranger, 404, api.ProblemNotFound},
		{"stranger transcript", "GET", jobPath + "/transcript", stranger, 404, api.ProblemNotFound},
		{"stranger result", "GET", jobPath + "/result", stranger, 404, api.ProblemNotFound},
		{"stranger deletes", "DELETE", jobPath, stranger, 404, api.ProblemNotFound},
		{"other queue reads", "GET", jobPath, privateOnly, 404, api.ProblemNotFound},
		{"no grants reads", "GET", jobPath, noGrants, 404, api.ProblemNotFound},
		{"unknown job", "GET", "/job/0193aaaa-0000-7000-8000-000000000000", h.admin, 404, api.ProblemNotFound},
		{"viewer deletes", "DELETE", jobPath, viewer, 403, api.ProblemForbidden},
		{"viewer transcript", "GET", jobPath + "/transcript", viewer, 403, api.ProblemForbidden},
		{"viewer result", "GET", jobPath + "/result", viewer, 403, api.ProblemForbidden},
		{"reader not done", "GET", jobPath + "/transcript", reader, 409, api.ProblemNotDone},
		{"reader deletes", "DELETE", jobPath, reader, 403, api.ProblemForbidden},
		// Queue access.
		{"no grants list", "GET", "/queue/public", noGrants, 403, api.ProblemForbidden},
		{"other queue list", "GET", "/queue/public", privateOnly, 403, api.ProblemForbidden},
		{"submit wrong queue", "POST", "/queue/private", owner, 403, api.ProblemForbidden},
		{"unknown queue", "GET", "/queue/nope", h.admin, 404, api.ProblemNotFound},
		// Leases belong to their holder.
		{"other worker input", "GET", l.InputURL, otherWTok, 404, api.ProblemNotFound},
		{"other worker release", "DELETE", "/lease/" + l.ID, otherWTok, 404, api.ProblemNotFound},
		{"other worker failure", "POST", "/lease/" + l.ID + "/failure", otherWTok, 404, api.ProblemNotFound},
		// Admin and client management.
		{"submitter lists workers", "GET", "/admin/workers", owner, 403, api.ProblemForbidden},
		{"submitter audit", "GET", "/admin/audit", owner, 403, api.ProblemForbidden},
		{"submitter users", "GET", "/admin/users", owner, 403, api.ProblemForbidden},
		{"submitter clients", "GET", "/admin/clients", owner, 403, api.ProblemForbidden},
		{"submitter creates queue", "POST", "/admin/queues", owner, 403, api.ProblemForbidden},
		{"submitter approves", "POST", "/admin/workers/" + wid + "/approve", owner, 403, api.ProblemForbidden},
		{"submitter pauses", "POST", "/admin/workers/" + wid + "/pause", owner, 403, api.ProblemForbidden},
		{"submitter creates client", "POST", "/client", owner, 403, api.ProblemForbidden},
		{"unknown route", "GET", "/nope", h.admin, 404, api.ProblemNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := strings.NewReader("{}")
			if c.method == "GET" || c.method == "DELETE" {
				body = strings.NewReader("")
			}
			resp := h.do(req{method: c.method, path: c.path, token: c.token, body: body})
			b := make([]byte, 4096)
			n, _ := resp.Body.Read(b)
			h.expectProblem(resp, b[:n], c.status, c.slug)
		})
	}
	// Denials are logged without leaking the token.
	denied := h.logs.events(t, obs.EventAuthDenied)
	assert.NotEmpty(t, denied)
	for _, d := range denied {
		assert.Contains(t, d, obs.FieldReason)
		assert.Contains(t, d, obs.FieldRoute)
	}
	obstest.AssertClean(t, h.logs.Bytes())
}

func TestClientEndpoints(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.expect("POST", "/admin/queues", h.admin, api.Queue{Name: "private"}, http.StatusCreated, nil)
	cadmin := h.client(api.Grant{Role: api.RoleClientAdmin, Scope: "*"}, submitOn("public"))

	var c api.Client
	h.expect("POST", "/client", cadmin, api.CreateClientRequest{Name: "harness",
		Grants: []api.Grant{submitOn("public")}}, http.StatusCreated, &c)
	assert.True(t, strings.HasPrefix(c.Token, api.ClientTokenPrefix))
	assert.Equal(t, []api.Grant{submitOn("public")}, c.Grants)

	// No escalation: a grant the creator does not hold.
	resp, b := h.call("POST", "/client", cadmin, api.CreateClientRequest{Name: "x",
		Grants: []api.Grant{submitOn("private")}})
	h.expectProblem(resp, b, http.StatusForbidden, api.ProblemForbidden)
	resp, b = h.call("POST", "/client", cadmin, api.CreateClientRequest{Name: "x",
		Grants: []api.Grant{{Role: api.RoleAdmin, Scope: "*"}}})
	h.expectProblem(resp, b, http.StatusForbidden, api.ProblemForbidden)

	// The new client works, may view itself but not others.
	var self api.Client
	h.expect("GET", "/client/"+c.ID, c.Token, nil, http.StatusOK, &self)
	assert.Empty(t, self.Token, "token shown only once")
	assert.NotNil(t, self.LastUsedAt)
	var list []api.Client
	h.expect("GET", "/admin/clients", cadmin, nil, http.StatusOK, &list)
	// Clients created in the same (fake-clock) millisecond list in id
	// order, which is random: pick one that is not c.
	other := list[0].ID
	if other == c.ID {
		other = list[1].ID
	}
	resp, b = h.call("GET", "/client/"+other, c.Token, nil)
	h.expectProblem(resp, b, http.StatusForbidden, api.ProblemForbidden)

	h.expect("DELETE", "/client/"+c.ID, cadmin, nil, http.StatusNoContent, nil)
	resp, b = h.call("GET", "/queue/public", c.Token, nil)
	h.expectProblem(resp, b, http.StatusUnauthorized, api.ProblemUnauthorized)
	obstest.AssertClean(t, h.logs.Bytes())
}

func TestQueuePatchAuthorization(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	qadmin := h.client(api.Grant{Role: api.RoleQueueAdmin, Scope: "queue:public"})
	desc, weight := "the public queue", 250
	var q api.Queue
	h.expect("PATCH", "/queue/public", qadmin, api.QueuePatch{Description: &desc}, http.StatusOK, &q)
	assert.Equal(t, desc, q.Description)
	resp, b := h.call("PATCH", "/queue/public", qadmin, api.QueuePatch{Weight: &weight})
	h.expectProblem(resp, b, http.StatusForbidden, api.ProblemForbidden)
	h.expect("PATCH", "/queue/public", h.admin, api.QueuePatch{Weight: &weight}, http.StatusOK, &q)
	assert.Equal(t, 250, q.Weight)
	bad := 0
	resp, b = h.call("PATCH", "/queue/public", h.admin, api.QueuePatch{Weight: &bad})
	h.expectProblem(resp, b, http.StatusBadRequest, api.ProblemBadRequest)

	var audit api.AuditList
	h.expect("GET", "/admin/audit", h.admin, nil, http.StatusOK, &audit)
	var actions []string
	for _, e := range audit.Entries {
		actions = append(actions, e.Action)
	}
	assert.Contains(t, actions, "queue.patch")
}

// TestApprovalNeedsBothRights: lifecycle (worker_admin) and data access
// (queue_admin) are separate acts (DESIGN.md §5.2).
func TestApprovalNeedsBothRights(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wadmin := h.client(api.Grant{Role: api.RoleWorkerAdmin, Scope: "*"})
	qadmin := h.client(api.Grant{Role: api.RoleQueueAdmin, Scope: "queue:public"})
	both := h.client(api.Grant{Role: api.RoleWorkerAdmin, Scope: "*"},
		api.Grant{Role: api.RoleQueueAdmin, Scope: "queue:public"})
	id, tok := h.worker(api.WorkerFacts{Hostname: "garage"})

	approve := api.ApproveWorkerRequest{Queues: []string{"public"}}
	resp, b := h.call("POST", "/admin/workers/"+id+"/approve", wadmin, approve)
	h.expectProblem(resp, b, http.StatusForbidden, api.ProblemForbidden)
	resp, b = h.call("POST", "/admin/workers/"+id+"/approve", qadmin, approve)
	h.expectProblem(resp, b, http.StatusForbidden, api.ProblemForbidden)
	resp, b = h.call("POST", "/admin/workers/"+id+"/approve", both, api.ApproveWorkerRequest{Queues: []string{"nope"}})
	h.expectProblem(resp, b, http.StatusForbidden, api.ProblemForbidden)
	h.expect("POST", "/admin/workers/"+id+"/approve", both, approve, http.StatusNoContent, nil)

	var w api.Worker
	h.expect("GET", "/worker/"+id, tok, nil, http.StatusOK, &w)
	assert.Equal(t, api.WorkerActive, w.State)
	assert.Equal(t, []string{"public"}, w.Queues)

	// The queue admin can see workers (to grant access) but not pause them.
	var ws []api.WorkerSummary
	h.expect("GET", "/admin/workers", qadmin, nil, http.StatusOK, &ws)
	require.Len(t, ws, 1)
	resp, b = h.call("POST", "/admin/workers/"+id+"/pause", qadmin, nil)
	h.expectProblem(resp, b, http.StatusForbidden, api.ProblemForbidden)

	approved := h.logs.events(t, obs.EventWorkerApproved)
	require.Len(t, approved, 1)
	assert.Equal(t, id, approved[0][obs.FieldWorkerID])
	assert.Equal(t, "garage", approved[0][obs.FieldHostname])
	assert.Equal(t, []any{"public"}, approved[0][obs.FieldQueues])
}

func TestAdminUsersGrantsAudit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var u api.User
	h.expect("POST", "/admin/users", h.admin, api.CreateUserRequest{Username: "alice",
		Grants: []api.Grant{{Role: api.RoleQueueAdmin, Scope: "queue:public"}}}, http.StatusCreated, &u)
	assert.Len(t, u.Password, 24, "generated password returned once")
	assert.True(t, u.MustChangePassword)
	assert.Equal(t, []api.Grant{{Role: api.RoleQueueAdmin, Scope: "queue:public"}}, u.Grants)
	resp, b := h.call("POST", "/admin/users", h.admin, api.CreateUserRequest{Username: "alice"})
	h.expectProblem(resp, b, http.StatusConflict, api.ProblemConflict)
	resp, b = h.call("POST", "/admin/users", h.admin, api.CreateUserRequest{Username: "Bad Name"})
	h.expectProblem(resp, b, http.StatusBadRequest, api.ProblemBadRequest)

	var users []api.User
	h.expect("GET", "/admin/users", h.admin, nil, http.StatusOK, &users)
	require.Len(t, users, 2)
	assert.Empty(t, users[1].Password)

	wid, _ := h.worker(api.WorkerFacts{}) // registered (and audited) before the grant changes below
	g := api.PrincipalGrant{Principal: "user:alice", Role: api.RoleViewAll, Scope: "queue:public"}
	h.expect("POST", "/admin/grants", h.admin, g, http.StatusNoContent, nil)
	var gs []api.PrincipalGrant
	h.expect("GET", "/admin/grants?principal=user:alice", h.admin, nil, http.StatusOK, &gs)
	assert.Len(t, gs, 2)
	h.expect("DELETE", "/admin/grants?principal=user:alice&role=view_all&scope=queue:public", h.admin, nil,
		http.StatusNoContent, nil)
	h.expect("GET", "/admin/grants?principal=user:alice", h.admin, nil, http.StatusOK, &gs)
	assert.Len(t, gs, 1)

	// Grants that make no sense are refused.
	for _, bad := range []api.PrincipalGrant{
		{Principal: "worker:" + wid, Role: api.RoleSubmit, Scope: "queue:public"},
		{Principal: "user:alice", Role: api.RoleProcess, Scope: "queue:public"},
		{Principal: "user:alice", Role: api.RoleSubmit, Scope: "queue:nope"},
	} {
		resp, b := h.call("POST", "/admin/grants", h.admin, bad)
		h.expectProblem(resp, b, http.StatusBadRequest, api.ProblemBadRequest)
	}
	resp, b = h.call("POST", "/admin/grants", h.admin, api.PrincipalGrant{Principal: "user:nobody",
		Role: api.RoleSubmit, Scope: "queue:public"})
	h.expectProblem(resp, b, http.StatusNotFound, api.ProblemNotFound)

	var audit api.AuditList
	h.expect("GET", "/admin/audit?limit=3", h.admin, nil, http.StatusOK, &audit)
	require.Len(t, audit.Entries, 3)
	assert.Equal(t, "grant.remove", audit.Entries[0].Action, "newest first")
	assert.NotZero(t, audit.NextBefore)
	var older api.AuditList
	h.expect("GET", "/admin/audit?before="+itoa64(audit.NextBefore), h.admin, nil, http.StatusOK, &older)
	assert.Less(t, older.Entries[0].ID, audit.NextBefore)
	assert.NotEmpty(t, h.logs.events(t, obs.EventAdminChange))
	obstest.AssertClean(t, h.logs.Bytes())
	assert.NotContains(t, string(h.logs.Bytes()), u.Password)
}

// TestSessionAuth covers the UI seam: login, session cookie on the API,
// the CSRF requirement for mutations, rotation and logout.
func TestSessionAuth(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var u api.User
	h.expect("POST", "/admin/users", h.admin, api.CreateUserRequest{Username: "bob", Password: "correct horse",
		Grants: []api.Grant{submitOn("public")}}, http.StatusCreated, &u)

	_, err := h.srv.Login(h.ctx, "bob", "wrong")
	assert.ErrorIs(t, err, ErrBadCredentials)
	_, err = h.srv.Login(h.ctx, "nobody", "x")
	assert.ErrorIs(t, err, ErrBadCredentials)
	user, err := h.srv.Login(h.ctx, "bob", "correct horse")
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	sess, err := h.srv.StartSession(rec, httptest.NewRequest("POST", "/ui/login", nil), user)
	require.NoError(t, err)
	cookie := rec.Result().Cookies()[0]
	assert.Equal(t, SessionCookieName, cookie.Name)
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)

	withCookie := func(method, path string, hdr map[string]string) *http.Response {
		r, _ := http.NewRequest(method, h.ts.URL+path, strings.NewReader(`{"description":"x"}`))
		r.AddCookie(cookie)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		resp, err := h.ts.Client().Do(r)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	assert.Equal(t, http.StatusOK, withCookie("GET", "/queue/public", nil).StatusCode)
	assert.Equal(t, http.StatusForbidden, withCookie("POST", "/client", nil).StatusCode, "mutations need CSRF")
	resp := withCookie("PATCH", "/queue/public", map[string]string{CSRFHeader: sess.CSRF})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "CSRF passes; bob is not queue_admin")
	var p api.Problem
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	assert.NotContains(t, p.Detail, "CSRF")

	// Request-side helpers.
	r := httptest.NewRequest("GET", "/ui/", nil)
	r.AddCookie(cookie)
	got, gotUser, err := h.srv.SessionFromRequest(r)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, got.ID)
	assert.Equal(t, "bob", gotUser.Username)
	pr, err := h.srv.RequestPrincipal(r)
	require.NoError(t, err)
	assert.Equal(t, "user:bob", pr.Name)
	assert.Equal(t, []api.Grant{submitOn("public")}, pr.Grants)

	// Logging in again rotates the session; logout clears it.
	rec = httptest.NewRecorder()
	_, err = h.srv.StartSession(rec, r, user)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, withCookie("GET", "/queue/public", nil).StatusCode, "old session gone")
	newCookie := rec.Result().Cookies()[0]
	r2 := httptest.NewRequest("POST", "/ui/logout", nil)
	r2.AddCookie(newCookie)
	rec = httptest.NewRecorder()
	require.NoError(t, h.srv.EndSession(rec, r2))
	s, _, err := h.srv.SessionFromRequest(r2)
	require.NoError(t, err)
	assert.Nil(t, s)
	obstest.AssertClean(t, h.logs.Bytes())
}

func TestRegistrationLimits(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) {
		c.RegisterBurst = 2
		c.MaxPendingWorkers = 3
	})
	facts := api.WorkerFacts{Hostname: "h", SandboxLevel: api.SandboxFull}
	h.expect("POST", "/worker", "", facts, http.StatusCreated, nil)
	h.expect("POST", "/worker", "", facts, http.StatusCreated, nil)
	resp, b := h.call("POST", "/worker", "", facts)
	h.expectProblem(resp, b, http.StatusTooManyRequests, api.ProblemRateLimited)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))

	// X-Forwarded-For is ignored unless the server trusts a proxy.
	r := h.do(req{method: "POST", path: "/worker", body: jsonBody(t, facts),
		header: map[string]string{"X-Forwarded-For": "203.0.113.9"}})
	assert.Equal(t, http.StatusTooManyRequests, r.StatusCode)

	resp, b = h.call("POST", "/worker", "", api.WorkerFacts{})
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "rate limit applies before validation: %s", b)

	trusting := newHarness(t, func(c *Config, _ *store.Config) {
		c.RegisterBurst = 1
		c.MaxPendingWorkers = 3
		c.TrustProxy = true
	})
	for i, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
		r := trusting.do(req{method: "POST", path: "/worker", body: jsonBody(t, facts),
			header: map[string]string{"X-Forwarded-For": "198.51.100.7, " + ip}})
		assert.Equal(t, http.StatusCreated, r.StatusCode, "registration %d", i)
	}
	r = trusting.do(req{method: "POST", path: "/worker", body: jsonBody(t, facts),
		header: map[string]string{"X-Forwarded-For": "203.0.113.4"}})
	assert.Equal(t, http.StatusServiceUnavailable, r.StatusCode, "pending cap reached")

	r = trusting.do(req{method: "POST", path: "/worker", body: strings.NewReader(`{"hostname":"x","sandbox_level":"full","bogus":1}`),
		header: map[string]string{"X-Forwarded-For": "203.0.113.5"}})
	assert.Equal(t, http.StatusServiceUnavailable, r.StatusCode,
		"unknown fields are tolerated in the worker protocol; the pending cap still applies")
}

func jsonBody(t *testing.T, v any) *strings.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return strings.NewReader(string(b))
}
