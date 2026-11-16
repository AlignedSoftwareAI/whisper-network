package ui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
)

// storeJob creates a canonical job on public owned by principal id owner,
// split into n transcribe tasks of one second each.
func (e *env) storeJob(owner string, n int) *store.Job {
	e.t.Helper()
	q, err := e.st.QueueByName(e.ctx, "public")
	require.NoError(e.t, err)
	ranges := make([]store.SampleRange, n)
	for i := range ranges {
		ranges[i] = store.SampleRange{Start: int64(i) * media.SampleRate, End: int64(i+1) * media.SampleRate}
	}
	j, _, err := e.st.CreateJob(e.ctx, store.NewJob{QueueID: q.ID, ClientID: owner,
		Params:    api.JobParams{Model: "small.en"},
		Original:  store.Media{Bytes: 100, SHA256: "o", Type: "audio/flac"},
		Canonical: &store.Canonical{Bytes: 100, SHA256: "c", Samples: int64(n) * media.SampleRate},
		Ranges:    ranges})
	require.NoError(e.t, err)
	return j
}

// claim makes a worker take one transcribe task through the API.
func (e *env) claim(workerID, token string) api.Lease {
	e.t.Helper()
	status, b := e.api("POST", "/worker/"+workerID+"/leases", token,
		api.ClaimRequest{Kinds: []api.TaskKind{api.KindTranscribe}, Models: []string{"small.en"}})
	require.Equal(e.t, http.StatusCreated, status, string(b))
	var l api.Lease
	require.NoError(e.t, json.Unmarshal(b, &l))
	return l
}

// TestJobWorkers: the queue's job table, the job page and the workers page
// show which workers hold a job's tasks. Worker names are shown only to
// admin, queue_admin on the job's queue and worker_admin over the worker;
// view_all and the job's owner see only that it is running. Worker
// descriptions are escaped.
func TestJobWorkers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const evil = `<script>alert("x")</script>`
	wa, waTok := e.worker("box-a", evil)
	wb, wbTok := e.worker("box-b", "") // shown by hostname
	for _, w := range []string{wa, wb} {
		require.NoError(t, e.srv.Ops().ApproveWorker(e.ctx, server.CLIActor(), w, []string{"public"}))
	}
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	e.user("qa", api.Grant{Role: api.RoleQueueAdmin, Scope: auth.QueueScope("public")})
	e.user("viewer", api.Grant{Role: api.RoleViewAll, Scope: auth.QueueScope("public")})
	e.user("sub", api.Grant{Role: api.RoleSubmit, Scope: auth.QueueScope("public")})
	e.user("wab", api.Grant{Role: api.RoleWorkerAdmin, Scope: auth.WorkerScope(wb)})
	sub, err := e.st.UserByUsername(e.ctx, "sub")
	require.NoError(t, err)

	// A finished job, run by wb, and a running one owned by sub with one
	// task on each worker.
	done := e.submit(e.admin, "public", "finished")
	e.runJob(wb, wbTok)
	running := e.storeJob(sub.Principal(), 2)
	la := e.claim(wa, waTok)
	lb := e.claim(wb, wbTok)
	require.Equal(t, []int{1, 2}, []int{la.Seq, lb.Seq})
	e.clk.Advance(20 * time.Second)
	status, b := e.api("POST", "/lease/"+la.ID+"/heartbeat", waTok, api.HeartbeatRequest{})
	require.Equal(t, http.StatusOK, status, string(b))
	e.clk.Advance(10 * time.Second)

	linkA, linkB := `href="/ui/workers/`+wa+`"`, `href="/ui/workers/`+wb+`"`
	escaped := `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;`

	t.Run("privileged see names", func(t *testing.T) {
		t.Parallel()
		for _, name := range []string{"root", "qa"} {
			br := e.login(name)
			resp, body := br.get("/ui/queues/public")
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Contains(t, body, ">Workers<button")
			assert.Contains(t, body, linkA, name)
			jobs := body[strings.Index(body, `id="jobs"`):]
			jobs = jobs[:strings.Index(jobs, "</table>")]
			assert.Equal(t, 2, strings.Count(jobs, linkB), "%s: wb runs one job and finished the other", name)
			assert.Contains(t, body, `>box-b <span class="muted">(`, name)
			assert.Contains(t, body, escaped, name)
			assert.NotContains(t, body, "<script>alert", name)
			assert.False(t, strings.Contains(body, `<span class="muted">running</span>`) ||
				strings.Contains(body, "2 running"), "%s sees names, not a count", name)

			resp, body = br.get("/ui/jobs/" + running.ID)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Contains(t, body, "Running tasks")
			assert.Contains(t, body, linkA)
			assert.Contains(t, body, linkB)
			assert.Contains(t, body, escaped)
			assert.NotContains(t, body, "<script>alert")
			assert.Contains(t, body, "10s ago", "wa's heartbeat")
			assert.Contains(t, body, "none yet", "wb has not heartbeated")
			assert.Contains(t, body, `<td class="n">30s</td>`, "lease age")

			resp, body = br.get("/ui/jobs/" + done.ID)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.NotContains(t, body, "Running tasks", "a done job has no running tasks")
		}
	})

	t.Run("others see only that it runs", func(t *testing.T) {
		t.Parallel()
		for _, name := range []string{"viewer", "sub"} {
			br := e.login(name)
			resp, body := br.get("/ui/queues/public")
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Contains(t, body, "/ui/jobs/"+running.ID, name)
			assert.Contains(t, body, "2 running", name)
			for _, leak := range []string{"/ui/workers/", "box-a", "box-b", escaped, wa, wb} {
				assert.NotContains(t, body, leak, name)
			}

			resp, body = br.get("/ui/jobs/" + running.ID)
			require.Equal(t, http.StatusOK, resp.StatusCode, name)
			assert.Contains(t, body, "Running tasks", name)
			assert.Contains(t, body, `<span class="muted">running</span>`, name)
			assert.Contains(t, body, "10s ago", name)
			for _, leak := range []string{"/ui/workers/", "box-a", "box-b", escaped} {
				assert.NotContains(t, body, leak, name)
			}
		}
	})

	t.Run("worker page lists leases", func(t *testing.T) {
		t.Parallel()
		br := e.login("root")
		resp, body := br.get("/ui/workers/" + wa)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, "<h2>Current leases</h2>")
		assert.Contains(t, body, `href="/ui/jobs/`+running.ID+`"`)
		assert.Contains(t, body, "#1 transcribe · 30s")
		assert.NotContains(t, body, "#2 transcribe")
		_, body = br.get("/ui/workers/" + wb)
		assert.Contains(t, body, "#2 transcribe · 30s")

		// A worker admin over wb alone sees its page, but not the job id
		// of a lease on a queue it cannot view.
		resp, body = e.login("wab").get("/ui/workers/" + wb)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, "1 on jobs you cannot view")
		assert.NotContains(t, body, running.ID)
		resp, _ = e.login("wab").get("/ui/workers/" + wa)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// TestJobWorkersAfterExpiry: a lease past its expiry is no longer shown as
// running, even before the reaper ends it.
func TestJobWorkersAfterExpiry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	wa, waTok := e.worker("box-a", "alpha")
	require.NoError(t, e.srv.Ops().ApproveWorker(e.ctx, server.CLIActor(), wa, []string{"public"}))
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	j := e.storeJob("someone", 1)
	e.claim(wa, waTok)
	br := e.login("root")
	_, body := br.get("/ui/jobs/" + j.ID)
	assert.Contains(t, body, `>alpha <span class="muted">(`)

	e.clk.Advance(time.Hour)
	_, body = br.get("/ui/jobs/" + j.ID)
	assert.NotContains(t, body, "Running tasks")
	_, body = br.get("/ui/queues/public")
	jobs := body[strings.Index(body, `id="jobs"`):]
	assert.NotContains(t, jobs[:strings.Index(jobs, "</table>")], `>alpha <span class="muted">(`)
	_, body = br.get("/ui/workers/" + wa)
	assert.NotContains(t, body, "/ui/jobs/"+j.ID)
}
