package ui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/server"
)

// failJob makes an approved worker claim the queue's next task and fail it
// for good, failing its job.
func (e *env) failJob(workerID, workerToken string) string {
	e.t.Helper()
	status, b := e.api("POST", "/worker/"+workerID+"/leases", workerToken,
		api.ClaimRequest{Kinds: []api.TaskKind{api.KindTranscribe}, Models: []string{"small.en"}})
	require.Equal(e.t, http.StatusCreated, status, string(b))
	var l api.Lease
	require.NoError(e.t, json.Unmarshal(b, &l))
	status, b = e.api("POST", "/lease/"+l.ID+"/failure", workerToken, api.FailureReport{Reason: "bad input"})
	require.Equal(e.t, http.StatusNoContent, status, string(b))
	return l.Job
}

func (e *env) jobState(id string) api.JobState {
	e.t.Helper()
	j, err := e.st.Job(e.ctx, id)
	require.NoError(e.t, err)
	return j.State
}

func (e *env) approved() (string, string) {
	e.t.Helper()
	wid, wtok := e.worker("runner", "")
	require.NoError(e.t, e.srv.Ops().ApproveWorker(e.ctx, server.CLIActor(), wid, []string{"public"}))
	return wid, wtok
}

// TestUIRetryJob: the job page offers Retry to whoever may retry a failed
// job with its media, says how long the files are kept, and retries (#56).
func TestUIRetryJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("qa", api.Grant{Role: api.RoleQueueAdmin, Scope: auth.QueueScope("public")})
	e.user("viewer", api.Grant{Role: api.RoleViewAll, Scope: auth.QueueScope("public")})
	tok := e.client(api.Grant{Role: api.RoleSubmit, Scope: auth.QueueScope("public")})
	wid, wtok := e.approved()
	e.submit(tok, "public", "x")
	id := e.failJob(wid, wtok)

	v := e.login("viewer")
	_, body := v.get("/ui/jobs/" + id)
	assert.Contains(t, body, "kept until")
	assert.NotContains(t, body, "Retry job")
	resp, _ := v.post("/ui/jobs/"+id+"/retry", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, api.JobFailed, e.jobState(id))

	b := e.login("qa")
	_, body = b.get("/ui/jobs/" + id)
	assert.Contains(t, body, "Retry job")
	resp, _ = b.post("/ui/jobs/"+id+"/retry", nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, api.JobQueued, e.jobState(id))
	_, body = b.get("/ui/jobs/" + id)
	assert.NotContains(t, body, "Retry job", "only failed jobs")
	assert.Contains(t, body, "Retries")

	// Once the media is gone the job page says so and offers no retry.
	id = e.failJob(wid, wtok)
	require.NoError(t, e.st.MarkCanonicalPurged(e.ctx, id))
	_, body = b.get("/ui/jobs/" + id)
	assert.Contains(t, body, "purged; the job cannot be retried")
	assert.NotContains(t, body, "Retry job")
	resp, body = b.post("/ui/jobs/"+id+"/retry", nil)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, body, "purged")
}

// TestUIPartialTranscriptLink: a failed job with finished tasks offers the
// partial transcript to a reader (#66).
func TestUIPartialTranscriptLink(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *server.Config) { c.TaskTarget = 3_000_000_000; c.SplitWindow = 500_000_000 })
	tok := e.client(api.Grant{Role: api.RoleSubmit, Scope: auth.QueueScope("public")})
	e.user("reader", api.Grant{Role: api.RoleReadAll, Scope: auth.QueueScope("public")})
	e.user("viewer", api.Grant{Role: api.RoleViewAll, Scope: auth.QueueScope("public")})
	job := e.submitAudio(tok, "public", "long", 10)
	require.Greater(t, job.Tasks.Total, 1)
	wid, wtok := e.approved()

	status, b := e.api("POST", "/worker/"+wid+"/leases", wtok,
		api.ClaimRequest{Kinds: []api.TaskKind{api.KindTranscribe}, Models: []string{"small.en"}})
	require.Equal(t, http.StatusCreated, status, string(b))
	var l api.Lease
	require.NoError(t, json.Unmarshal(b, &l))
	status, b = e.api("PUT", "/lease/"+l.ID+"/result", wtok, api.TranscribeResult{EngineVersion: "whisper.cpp 1.9.5",
		Model: l.Model, ModelSHA256: l.ModelSHA256, VAD: l.VADModel, Language: "en", ComputeMS: 1,
		Segments: []api.Segment{{StartMS: 0, EndMS: 900, Text: "hello"}}})
	require.Less(t, status, 300, string(b))

	_, body := e.login("reader").get("/ui/jobs/" + job.ID)
	assert.Contains(t, body, "/job/"+job.ID+"/transcript?partial=1")
	_, body = e.login("viewer").get("/ui/jobs/" + job.ID)
	assert.NotContains(t, body, "partial=1", "viewing is not reading")
}

// TestUIBulkRetryAndDelete: checkboxes post the selection, the server asks
// for confirmation with the count, then reports per-job outcomes and a
// summary; "all matching" acts on the filter (#68).
func TestUIBulkRetryAndDelete(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tok := e.client(api.Grant{Role: api.RoleSubmit, Scope: auth.QueueScope("public")})
	e.user("qa", api.Grant{Role: api.RoleQueueAdmin, Scope: auth.QueueScope("public")})
	wid, wtok := e.approved()
	var failed []string
	for range 3 {
		e.submit(tok, "public", "x")
		failed = append(failed, e.failJob(wid, wtok))
	}
	require.NoError(t, e.st.MarkCanonicalPurged(e.ctx, failed[2]))

	b := e.login("qa")
	_, body := b.get("/ui/queues/public?state=failed")
	assert.Contains(t, body, `name="job_id" value="`+failed[0]+`"`)
	assert.Contains(t, body, "data-select-page")
	assert.Contains(t, body, "Retry all 3")
	assert.Contains(t, body, "/ui/static/queue.js")

	sel := url.Values{"op": {"retry-selected"}, "job_id": {failed[0], failed[2]}}
	resp, body := b.post("/ui/queues/public/bulk", sel)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, "Retry <strong>2</strong>")
	assert.Equal(t, api.JobFailed, e.jobState(failed[0]), "nothing happens before confirming")

	sel.Set("confirm", "1")
	resp, body = b.post("/ui/queues/public/bulk", sel)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, "1 retried, 1 skipped: media purged")
	assert.Equal(t, api.JobQueued, e.jobState(failed[0]))

	all := url.Values{"op": {"delete-all"}, "state": {"failed"}}
	_, body = b.post("/ui/queues/public/bulk", all)
	assert.Contains(t, body, "Delete <strong>2</strong>")
	all.Set("confirm", "1")
	_, body = b.post("/ui/queues/public/bulk", all)
	assert.Contains(t, body, "2 deleted")
	assert.Equal(t, api.JobQueued, e.jobState(failed[0]), "only the filter's jobs")
	_, err := e.st.Job(e.ctx, failed[1])
	assert.Error(t, err)

	resp, _ = b.post("/ui/queues/public/bulk", url.Values{"op": {"retry-selected"}})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp, _ = b.post("/ui/queues/public/bulk", url.Values{"op": {"retry-all"}})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestUIBulkForbidden: a viewer's bulk delete is reported per job and
// changes nothing.
func TestUIBulkForbidden(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tok := e.client(api.Grant{Role: api.RoleSubmit, Scope: auth.QueueScope("public")})
	e.user("viewer", api.Grant{Role: api.RoleViewAll, Scope: auth.QueueScope("public")})
	wid, wtok := e.approved()
	e.submit(tok, "public", "x")
	id := e.failJob(wid, wtok)
	_, body := e.login("viewer").post("/ui/queues/public/bulk",
		url.Values{"op": {"delete-selected"}, "job_id": {id}, "confirm": {"1"}})
	assert.Contains(t, body, "1 forbidden")
	assert.Equal(t, api.JobFailed, e.jobState(id))
}

// TestUIQueueSort: the queue page orders by state by default, takes the
// order control, and remembers the choice in a cookie (#67).
func TestUIQueueSort(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	tok := e.client(api.Grant{Role: api.RoleSubmit, Scope: auth.QueueScope("public")})
	wid, wtok := e.approved()
	e.submit(tok, "public", "x")
	failedID := e.failJob(wid, wtok)
	e.clk.Advance(1_000_000_000)
	queued := e.submit(tok, "public", "y")

	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	b := e.login("root")
	first := func(body string) string {
		i, j := strings.Index(body, failedID), strings.Index(body, queued.ID)
		require.True(t, i >= 0 && j >= 0)
		if i < j {
			return "failed"
		}
		return "queued"
	}
	_, body := b.get("/ui/queues/public")
	assert.Equal(t, "queued", first(body), "by state: waiting before failed")
	assert.Contains(t, body, `<option value="state" selected>`)

	_, body = b.get("/ui/queues/public?order_by=created-asc")
	assert.Equal(t, "failed", first(body))
	_, body = b.get("/ui/queues/public")
	assert.Equal(t, "failed", first(body), "remembered")
	assert.Contains(t, body, `<option value="created-asc" selected>`)
	_, body = b.get("/ui/queues/public?sort=created&order=desc")
	assert.Equal(t, "queued", first(body))
	resp, _ := b.get("/ui/queues/public?order_by=size")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
