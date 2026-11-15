package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/media"
)

// TestReadAllSeparatesContentFromStatus: on a done job, view_all and
// queue_admin get the status document without prompt and metadata, and 403
// on the transcript and result (the job is visible, so not 404); read_all,
// the owner and admin get everything. The same redaction applies to the
// queue listing.
func TestReadAllSeparatesContentFromStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	viewPub := api.Grant{Role: api.RoleViewAll, Scope: "queue:public"}
	owner := h.client(submitOn("public"))
	viewer := h.client(viewPub)
	queueAdmin := h.client(api.Grant{Role: api.RoleQueueAdmin, Scope: "queue:public"})
	reader := h.client(api.Grant{Role: api.RoleReadAll, Scope: "queue:public"})
	both := h.client(viewPub, api.Grant{Role: api.RoleReadAll, Scope: "queue:public"})

	const prompt = "Alice Chen and Bob Rivera discuss the launch."
	meta := json.RawMessage(`{"requester":"carol"}`)
	job := h.submit(owner, "public", api.JobParams{Model: "small.en", Prompt: prompt, Metadata: meta},
		canonicalWAV(speech(1, media.SampleRate)))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	for {
		l := h.claim(wid, wtok, "", nil)
		if l == nil {
			break
		}
		resp, b := h.putResult(l, wtok, result(l, "hello"))
		require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	}
	require.Equal(t, api.JobDone, h.job(job.ID).State)

	for _, tc := range []struct {
		name  string
		token string
		reads bool
	}{
		{"owner", owner, true},
		{"view_all", viewer, false},
		{"queue_admin", queueAdmin, false},
		{"read_all", reader, true},
		{"view_all + read_all", both, true},
		{"admin", h.admin, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc api.Job
			h.expect("GET", "/job/"+job.ID, tc.token, nil, http.StatusOK, &doc)
			assert.Equal(t, api.JobDone, doc.State, "status is visible")
			var list api.JobList
			h.expect("GET", "/queue/public", tc.token, nil, http.StatusOK, &list)
			require.Len(t, list.Jobs, 1)
			if tc.reads {
				assert.Equal(t, prompt, doc.Prompt)
				assert.JSONEq(t, string(meta), string(doc.Metadata))
				assert.Equal(t, prompt, list.Jobs[0].Prompt)
				for _, out := range []string{"transcript", "result"} {
					resp, b := h.call("GET", "/job/"+job.ID+"/"+out, tc.token, nil)
					assert.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", out, b)
				}
				return
			}
			assert.Empty(t, doc.Prompt)
			assert.Empty(t, doc.Metadata)
			assert.Empty(t, list.Jobs[0].Prompt)
			assert.Empty(t, list.Jobs[0].Metadata)
			for _, out := range []string{"transcript", "result"} {
				resp, b := h.call("GET", "/job/"+job.ID+"/"+out, tc.token, nil)
				h.expectProblem(resp, b, http.StatusForbidden, api.ProblemForbidden)
			}
		})
	}
	// Denied attempts are not downloads.
	assert.Equal(t, 8, h.job(job.ID).Downloads.Count, "four readers, two outputs each")
}
