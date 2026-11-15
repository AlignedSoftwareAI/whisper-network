package program

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/e2e/e2etest"
	"github.com/deweysasser/whisper-network/internal/server"
)

// TestCLIAgainstRealServer (e2e scenario 11): submit, status and fetch
// against the real server, with a real worker doing the job.
//
// Every command line is parsed before the server starts: Parse sets
// zerolog's global field names (AfterApply), which the in-process server
// and worker read on every log line. A real process parses once, first.
func TestCLIAgainstRealServer(t *testing.T) {
	skipOnWindows(t, "runs a real worker")
	ctx := context.Background()
	media := filepath.Join(t.TempDir(), "meeting.wav")
	require.NoError(t, os.WriteFile(media, e2etest.SpeechWAV(3), 0o600))
	dst := filepath.Join(t.TempDir(), "out.txt")
	flags := []string{"--server", "http://placeholder.invalid", "--token", "wnc_placeholder"}
	mustParse := func(args ...string) *Options {
		t.Helper()
		opts, err := parse(t, append(append(args[:1:1], flags...), args[1:]...)...)
		require.NoError(t, err)
		return opts
	}
	submit := mustParse("submit", "--model", e2etest.Model.Name, "--weight", "high", "--label", "cli-run",
		"--metadata", `{"k":"v"}`, "--idempotency-key", "k1", media).Submit
	resubmit := mustParse("submit", "--model", e2etest.Model.Name, "--idempotency-key", "k1", "--json", media).Submit
	status := mustParse("status", "JOB").Status
	list := mustParse("status", "--state", "done").Status
	fetch := mustParse("fetch", "-o", dst, "JOB").Fetch
	fetchJSON := mustParse("fetch", "--json", "JOB").Fetch

	e := e2etest.New(t)
	w := e.StartWorker("cli-worker")
	e.Approve(w.ID(), "public")
	c, err := e.Server().Ops().CreateClient(ctx, server.CLIActor(), "cli",
		[]api.Grant{{Role: api.RoleSubmit, Scope: "queue:public"}})
	require.NoError(t, err)
	for _, f := range []*ClientFlags{&submit.ClientFlags, &resubmit.ClientFlags, &status.ClientFlags,
		&list.ClientFlags, &fetch.ClientFlags, &fetchJSON.ClientFlags} {
		f.Server, f.Token = e.URL, c.Token
	}

	var out bytes.Buffer
	submit.out = &out
	require.NoError(t, submit.Run(ctx))
	id := strings.TrimSpace(out.String())
	require.NotEmpty(t, id)

	// Resubmitting with the same key returns the same job.
	out.Reset()
	resubmit.out = &out
	require.NoError(t, resubmit.Run(ctx))
	var again api.Job
	require.NoError(t, json.Unmarshal(out.Bytes(), &again))
	assert.Equal(t, id, again.ID)
	assert.Equal(t, api.WeightHigh, again.Weight)

	e.WaitJob(id, api.JobDone)

	out.Reset()
	status.JobID, status.out = id, &out
	require.NoError(t, status.Run(ctx))
	var st api.Job
	require.NoError(t, json.Unmarshal(out.Bytes(), &st))
	assert.Equal(t, api.JobDone, st.State)
	assert.Equal(t, "cli-run", st.Label)
	assert.JSONEq(t, `{"k":"v"}`, string(st.Metadata))

	out.Reset()
	list.out = &out
	require.NoError(t, list.Run(ctx))
	assert.Regexp(t, id+`\s+done\s+high\s+e2e\.en\s+1/1\s+3s\s+.*cli-run`, out.String())

	fetch.JobID = id
	require.NoError(t, fetch.Run(ctx))
	txt, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(txt), "# transcript-source: whisper; "), string(txt))
	assert.Contains(t, string(txt), "job="+id)

	out.Reset()
	fetchJSON.JobID, fetchJSON.out = id, &out
	require.NoError(t, fetchJSON.Run(ctx))
	var res api.JobResult
	require.NoError(t, json.Unmarshal(out.Bytes(), &res))
	assert.Equal(t, id, res.JobID)
	assert.NotEmpty(t, res.Segments)

	j := e.Job(id)
	assert.Equal(t, 2, j.Downloads.Count, "both fetches were recorded")
}
