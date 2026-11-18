package ui

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/store"
)

func workerGone(t *testing.T, e *env, id string) bool {
	t.Helper()
	_, err := e.st.WorkerByID(e.ctx, id)
	if err == nil {
		return false
	}
	require.ErrorIs(t, err, store.ErrNotFound)
	return true
}

func reject(t *testing.T, e *env, b *browser, id string) {
	t.Helper()
	resp, _ := b.post("/ui/workers/"+id+"/reject", nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
}

// TestDeleteWorker: a rejected worker's page offers Delete, confirmed;
// deleting it returns to the workers page and audits worker.delete. A
// worker in any other state has no Delete and refuses one with 409.
func TestDeleteWorker(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	wid, _ := e.worker("box", "a box")
	b := e.login("root")

	_, body := b.get("/ui/workers/" + wid)
	assert.NotContains(t, body, ">Delete<", "only a rejected worker can be deleted")
	resp, _ := b.post("/ui/workers/"+wid+"/delete", nil)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.False(t, workerGone(t, e, wid))

	reject(t, e, b, wid)
	_, body = b.get("/ui/workers/" + wid)
	assert.Contains(t, body, ">Delete<")
	assert.Contains(t, body, `hx-confirm="Delete this worker now?`)

	resp, _ = b.post("/ui/workers/"+wid+"/delete", nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/workers", resp.Header.Get("Location"))
	assert.True(t, workerGone(t, e, wid))
	es, err := e.st.QueryAudit(e.ctx, store.AuditFilter{ActionPrefix: "worker.delete"})
	require.NoError(t, err)
	require.Len(t, es, 1)
	assert.Equal(t, store.WorkerPrincipal(wid), es[0].Target)

	resp, _ = b.post("/ui/workers/"+wid+"/delete", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestDeleteWorkerScope: deleting needs the right that rejects the worker,
// so worker_admin over another worker gets 403.
func TestDeleteWorkerScope(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	mine, _ := e.worker("mine", "")
	theirs, _ := e.worker("theirs", "")
	e.user("wa", api.Grant{Role: api.RoleWorkerAdmin, Scope: store.WorkerPrincipal(mine)})
	root := e.login("root")
	reject(t, e, root, mine)
	reject(t, e, root, theirs)

	wa := e.login("wa")
	resp, _ := wa.post("/ui/workers/"+theirs+"/delete", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.False(t, workerGone(t, e, theirs))
	resp, _ = wa.post("/ui/workers/"+mine+"/delete", nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.True(t, workerGone(t, e, mine))
}

// TestDeleteAllRejected: the Workers page's bulk action deletes only the
// rejected workers the viewer may manage, one worker.delete entry each,
// and is offered (with the count) only when there are some.
func TestDeleteAllRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	root := e.login("root")
	a, _ := e.worker("a", "")
	bw, _ := e.worker("b", "")
	other, _ := e.worker("other", "")
	reject(t, e, root, other) // frees a pending slot for this address
	live, _ := e.worker("live", "")
	e.user("wa", api.Grant{Role: api.RoleWorkerAdmin, Scope: store.WorkerPrincipal(a)},
		api.Grant{Role: api.RoleWorkerAdmin, Scope: store.WorkerPrincipal(bw)},
		api.Grant{Role: api.RoleWorkerAdmin, Scope: store.WorkerPrincipal(live)})
	wa := e.login("wa")

	_, body := wa.get("/ui/workers")
	assert.NotContains(t, body, "Delete all rejected", "the only rejected worker is out of scope")

	for _, id := range []string{a, bw} {
		reject(t, e, root, id)
	}
	_, body = wa.get("/ui/workers")
	assert.Contains(t, body, "Delete all rejected (2)", "counts only the viewer's rejected workers")
	assert.Contains(t, body, `hx-confirm="Delete 2 rejected workers now?`)
	_, body = wa.get("/ui/workers?state=active")
	assert.Contains(t, body, "Delete all rejected (2)", "the count ignores the filter")

	resp, _ := wa.post("/ui/workers/delete-rejected", nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/workers", resp.Header.Get("Location"))
	assert.True(t, workerGone(t, e, a))
	assert.True(t, workerGone(t, e, bw))
	assert.False(t, workerGone(t, e, live), "not rejected")
	assert.False(t, workerGone(t, e, other), "out of the viewer's scope")
	es, err := e.st.QueryAudit(e.ctx, store.AuditFilter{ActionPrefix: "worker.delete"})
	require.NoError(t, err)
	assert.Len(t, es, 2, "one entry per worker")

	_, body = wa.get("/ui/workers")
	assert.NotContains(t, body, "Delete all rejected")
	_, body = root.get("/ui/workers")
	assert.Contains(t, body, "Delete all rejected (1)")
}
