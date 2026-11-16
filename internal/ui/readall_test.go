package ui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/server"
)

// TestJobPageNeedsReadAccess: view_all and queue_admin see everything
// describing a job (status, prompt, submitter) but no download links, and
// the downloads themselves are refused; read_all and admin can also read
// the transcript.
func TestJobPageNeedsReadAccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pub := auth.QueueScope("public")
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	e.user("viewer", api.Grant{Role: api.RoleViewAll, Scope: pub})
	e.user("qa", api.Grant{Role: api.RoleQueueAdmin, Scope: pub})
	e.user("reader", api.Grant{Role: api.RoleReadAll, Scope: pub})
	tok := e.client(api.Grant{Role: api.RoleSubmit, Scope: pub})
	const prompt = "Alice Chen and Bob Rivera discuss the launch."
	j := e.submitParams(tok, "public", api.JobParams{Model: "small.en", Label: "planning",
		Prompt: prompt, Metadata: json.RawMessage(`{"requester":"carol"}`)}, 2)
	wid, wtok := e.worker("runner", "")
	require.NoError(t, e.srv.Ops().ApproveWorker(e.ctx, server.CLIActor(), wid, []string{"public"}))
	e.runJob(wid, wtok)

	for _, tc := range []struct {
		user  string
		reads bool
	}{{"viewer", false}, {"qa", false}, {"reader", true}, {"root", true}} {
		t.Run(tc.user, func(t *testing.T) {
			b := e.login(tc.user)
			resp, body := b.get("/ui/jobs/" + j.ID)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			assert.Contains(t, body, "planning", "the status is visible")
			assert.Contains(t, body, prompt, "the prompt is visible to anyone who sees the job")
			assert.Contains(t, body, "Submitted by")
			download := http.StatusForbidden
			if tc.reads {
				download = http.StatusOK
				assert.Contains(t, body, "Download transcript")
				assert.Contains(t, body, `href="/job/`+j.ID+`/result"`)
			} else {
				assert.NotContains(t, body, "Download transcript")
				assert.NotContains(t, body, `href="/job/`+j.ID+`/result"`)
			}
			_, body = b.get("/ui/queues/public")
			assert.Contains(t, body, "planning", "the job is listed")
			for _, out := range []string{"transcript", "result"} {
				resp, _ = b.get("/job/" + j.ID + "/" + out)
				assert.Equal(t, download, resp.StatusCode, out)
			}
		})
	}
}

// TestOnlyAdminGrantsReadAll: a queue admin (here also a client admin) is
// not offered read_all and is refused if it tries anyway; an admin is
// offered it and may grant and revoke it.
func TestOnlyAdminGrantsReadAll(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pub := auth.QueueScope("public")
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	e.user("qa", api.Grant{Role: api.RoleQueueAdmin, Scope: pub}, api.Grant{Role: api.RoleClientAdmin, Scope: "*"})
	e.user("bob")
	grant := url.Values{"principal": {"user:bob"}, "role": {"read_all"}, "scope": {"queue:public"}}

	qa := e.login("qa")
	_, body := qa.get("/ui/queues/public")
	assert.Contains(t, body, "<option>view_all</option>")
	assert.NotContains(t, body, "<option>read_all</option>")
	resp, body := qa.get("/ui/clients")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, `aria-label="View all on public"`)
	assert.NotContains(t, body, "Read transcripts", "no column for a role the editor cannot give")
	resp, _ = qa.post("/ui/grants", grant)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	root := e.login("root")
	_, body = root.get("/ui/queues/public")
	assert.Contains(t, body, "<option>read_all</option>")
	_, body = root.get("/ui/clients")
	assert.Contains(t, body, "Read transcripts")
	assert.Contains(t, body, `aria-label="Read transcripts on public"`)
	resp, _ = root.post("/ui/grants", grant)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	resp, _ = qa.post("/ui/grants/revoke", grant)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "revoking needs the same authority as granting")
	resp, _ = root.post("/ui/grants/revoke", grant)
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
}
