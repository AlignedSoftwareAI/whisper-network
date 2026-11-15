package ui

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/server"
)

// TestWorkersPageStats: the workers page summarises each worker's counters
// in a table (with the breakdown on hover) and details them on its card.
func TestWorkersPageStats(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	client := e.client(api.Grant{Role: api.RoleSubmit, Scope: auth.QueueScope("public")})
	e.submitAudio(client, "public", "talk", 36)
	wid, wtok := e.worker("runner", "")
	require.NoError(t, e.srv.Ops().ApproveWorker(e.ctx, server.CLIActor(), wid, []string{"public"}))
	e.worker("idle-host", "")
	e.runJob(wid, wtok)

	resp, body := e.login("root").get("/ui/workers")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	collapse := regexp.MustCompile(`\s+`)
	body = collapse.ReplaceAllString(body, " ")
	for _, want := range []string{
		`<table class="data worker-stats">`,
		`<th class="n">Tasks done</th>`, `<th class="n">Audio hours</th>`, `>Avg RTF</th>`, `>Failure rate</th>`,
		// runner: one 36 s transcribe task in 1 s of compute.
		`<td><a href="#w-` + wid + `">runner</a></td>`,
		`title="0 prepare · 1 transcribe · 1 jobs">1</td>`,
		`<td class="n">0.01</td>`,
		`<td class="n">36.0×</td>`,
		`title="0 failed · 0 expired">0%</td>`,
		// Its card.
		`1 tasks (0 prepare, 1 transcribe) in 1 jobs · 0.01 h audio · 1s compute · 36.0× realtime · 0 failed, 0 expired · first task`,
		// idle-host has done nothing: no rate to show.
		`title="0 failed · 0 expired">—</td>`,
	} {
		assert.Contains(t, body, want)
	}
}
