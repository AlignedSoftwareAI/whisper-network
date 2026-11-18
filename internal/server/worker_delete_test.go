package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/store"
)

// TestAdminDeleteWorker: DELETE /admin/workers/{id} deletes a rejected
// worker with its grants and counters, audits worker.delete by the acting
// principal with the final counters, and leaves its token unknown (401
// instead of 410).
func TestAdminDeleteWorker(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	require.NotNil(t, l)
	resp, b := h.putResult(l, wtok, result(l, "hello"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	scopedID, scoped := h.newClient(api.Grant{Role: api.RoleWorkerAdmin, Scope: store.WorkerPrincipal(wid)})

	h.expect("POST", "/admin/workers/"+wid+"/reject", h.admin, nil, http.StatusNoContent, nil)
	resp, b = h.call("GET", "/worker/"+wid, wtok, nil)
	h.expectProblem(resp, b, http.StatusGone, api.ProblemWorkerRejected)

	mark := h.auditMark()
	h.expect("DELETE", "/admin/workers/"+wid, scoped, nil, http.StatusNoContent, nil)
	e := h.expectOneAudit(mark, "worker.delete", "client:"+scopedID, store.WorkerPrincipal(wid))
	var d map[string]any
	require.NoError(t, json.Unmarshal([]byte(e.Detail), &d))
	assert.Equal(t, "rejected", d["state"])
	require.Contains(t, d, "stats", "the final counters survive the record")
	assert.EqualValues(t, 1, d["stats"].(map[string]any)["transcribe_tasks"])

	_, err := h.st.WorkerByID(h.ctx, wid)
	require.ErrorIs(t, err, store.ErrNotFound)
	gs, err := h.st.GrantsFor(h.ctx, store.WorkerPrincipal(wid))
	require.NoError(t, err)
	assert.Empty(t, gs, "its process grants are gone")

	for _, c := range []struct{ method, path string }{{"GET", "/worker/" + wid}, {"POST", "/worker/" + wid + "/leases"}} {
		resp, b = h.call(c.method, c.path, wtok, nil)
		h.expectProblem(resp, b, http.StatusUnauthorized, api.ProblemUnauthorized)
	}

	resp, b = h.call("DELETE", "/admin/workers/"+wid, h.admin, nil)
	h.expectProblem(resp, b, http.StatusNotFound, api.ProblemNotFound)
}

// TestAdminDeleteWorkerRefused: only a rejected worker may be deleted (409
// otherwise, and the worker stays), and only by worker_admin over it or
// admin (403).
func TestAdminDeleteWorkerRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	pending, _ := h.worker(api.WorkerFacts{Hostname: "pending"})
	active, _ := h.activeWorker(api.WorkerFacts{Hostname: "active"}, "public")
	paused, _ := h.activeWorker(api.WorkerFacts{Hostname: "paused"}, "public")
	h.expect("POST", "/admin/workers/"+paused+"/pause", h.admin, nil, http.StatusNoContent, nil)
	rejected, _ := h.worker(api.WorkerFacts{Hostname: "rejected"})
	h.expect("POST", "/admin/workers/"+rejected+"/reject", h.admin, nil, http.StatusNoContent, nil)

	for _, id := range []string{pending, active, paused} {
		resp, b := h.call("DELETE", "/admin/workers/"+id, h.admin, nil)
		h.expectProblem(resp, b, http.StatusConflict, api.ProblemConflict)
		_, err := h.st.WorkerByID(h.ctx, id)
		require.NoError(t, err, "a refused delete keeps the worker")
	}

	for name, tok := range map[string]string{
		"worker_admin over another worker": h.client(api.Grant{Role: api.RoleWorkerAdmin, Scope: store.WorkerPrincipal(active)}),
		"queue_admin":                      h.client(api.Grant{Role: api.RoleQueueAdmin, Scope: "queue:public"}),
		"submitter":                        h.client(submitOn("public")),
	} {
		resp, b := h.call("DELETE", "/admin/workers/"+rejected, tok, nil)
		h.expectProblem(resp, b, http.StatusForbidden, api.ProblemForbidden)
		_, err := h.st.WorkerByID(h.ctx, rejected)
		require.NoError(t, err, name)
	}
	h.expect("DELETE", "/admin/workers/"+rejected,
		h.client(api.Grant{Role: api.RoleWorkerAdmin, Scope: "*"}), nil, http.StatusNoContent, nil)
}
