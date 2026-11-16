package program

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

// bulkServer emulates POST /job/{id}/retry, DELETE /job/{id}, the partial
// transcript and POST /queue/{q}/bulk (two pages), recording the bulk
// requests.
func bulkServer(t *testing.T, reqs *[]api.BulkRequest) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /job/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Job{ID: r.PathValue("id"), State: api.JobQueued, Retries: 1})
	})
	mux.HandleFunc("DELETE /job/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /job/{id}/transcript", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "1", r.URL.Query().Get("partial"))
		_, _ = io.WriteString(w, "# x; partial=true; missing=1/2\n[... missing 0:00:00-0:01:00 (task 1) ...]\n")
	})
	mux.HandleFunc("POST /queue/{q}/bulk", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer "+cliToken, r.Header.Get("Authorization"))
		var req api.BulkRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		*reqs = append(*reqs, req)
		res := api.BulkResult{Action: req.Action, DryRun: req.DryRun, Counts: map[string]int{}}
		outcome := api.OutcomeRetried
		if req.DryRun {
			outcome = api.OutcomeWouldRetry
		}
		if req.Filter.Cursor == "" {
			res.Jobs = []api.BulkJobResult{{JobID: "j1", Outcome: outcome},
				{JobID: "j2", Outcome: api.OutcomeSkipped, Reason: "media purged"}}
			res.NextCursor = "c2"
		} else {
			res.Jobs = []api.BulkJobResult{{JobID: "j3", Outcome: api.OutcomeForbidden}}
		}
		for _, j := range res.Jobs {
			res.Counts[j.Outcome]++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRetryOneJob(t *testing.T) {
	var reqs []api.BulkRequest
	srv := bulkServer(t, &reqs)
	opts := parseCLI(t, srv, "retry", "j9")
	var out bytes.Buffer
	opts.Retry.out = &out
	require.NoError(t, opts.Retry.Run(context.Background()))
	assert.Equal(t, "j9 queued (retry 1)\n", out.String())
	assert.Empty(t, reqs)

	opts = parseCLI(t, srv, "retry", "--dry-run", "j9")
	assert.Error(t, opts.Retry.Run(context.Background()))
}

func TestRetryQueueFollowsPages(t *testing.T) {
	var reqs []api.BulkRequest
	srv := bulkServer(t, &reqs)
	opts := parseCLI(t, srv, "retry", "--queue", "media", "--since", "2026-11-15T10:00:00Z", "--dry-run")
	var out bytes.Buffer
	opts.Retry.out = &out
	require.NoError(t, opts.Retry.Run(context.Background()))
	require.Len(t, reqs, 2)
	since := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	assert.Equal(t, api.BulkRequest{Action: api.BulkRetry, DryRun: true,
		Filter: &api.BulkFilter{State: api.JobFailed, Since: &since}}, reqs[0])
	assert.Equal(t, "c2", reqs[1].Filter.Cursor)
	assert.Contains(t, out.String(), "j2  skipped      media purged")
	assert.Contains(t, out.String(), "1 forbidden, 1 skipped, 1 would retry\n")
}

func TestDeleteCommand(t *testing.T) {
	var reqs []api.BulkRequest
	srv := bulkServer(t, &reqs)
	opts := parseCLI(t, srv, "delete", "--state", "failed")
	var out bytes.Buffer
	opts.Delete.out = &out
	require.NoError(t, opts.Delete.Run(context.Background()))
	require.Len(t, reqs, 2)
	assert.Equal(t, api.BulkDelete, reqs[0].Action)
	assert.Equal(t, api.JobFailed, reqs[0].Filter.State)
	assert.False(t, reqs[0].DryRun)

	opts = parseCLI(t, srv, "delete")
	assert.ErrorContains(t, opts.Delete.Run(context.Background()), "--state")
	opts = parseCLI(t, srv, "delete", "j1")
	out.Reset()
	opts.Delete.out = &out
	require.NoError(t, opts.Delete.Run(context.Background()))
	assert.Equal(t, "j1 deleted\n", out.String())
}

func TestFetchPartial(t *testing.T) {
	var reqs []api.BulkRequest
	srv := bulkServer(t, &reqs)
	opts := parseCLI(t, srv, "fetch", "--partial", "j1")
	var out bytes.Buffer
	opts.Fetch.out = &out
	require.NoError(t, opts.Fetch.Run(context.Background()))
	assert.Contains(t, out.String(), "partial=true")
}
