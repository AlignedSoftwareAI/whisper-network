package ui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
)

// workerFleet has one worker in each state, an admin and a scoped
// worker_admin.
type workerFleet struct {
	*env
	pending, active, paused, rejected string
}

func newWorkerFleet(t *testing.T) *workerFleet {
	t.Helper()
	f := &workerFleet{env: newEnv(t)}
	// Registered in reverse display order, so sorting is what puts pending
	// first, and decided one by one: the server caps pending workers per
	// address.
	ops, cli := f.srv.Ops(), server.CLIActor()
	f.rejected, _ = f.worker("rejected-host", "")
	require.NoError(t, ops.SetWorkerState(f.ctx, cli, f.rejected, api.WorkerRejected))
	f.paused, _ = f.worker("paused-host", "")
	require.NoError(t, ops.ApproveWorker(f.ctx, cli, f.paused, []string{"public"}))
	require.NoError(t, ops.SetWorkerState(f.ctx, cli, f.paused, api.WorkerPaused))
	f.active, _ = f.worker("active-host", "")
	require.NoError(t, ops.ApproveWorker(f.ctx, cli, f.active, []string{"public"}))
	f.pending, _ = f.worker("pending-host", "")
	f.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	f.user("wa", api.Grant{Role: api.RoleWorkerAdmin, Scope: auth.WorkerScope(f.active)})
	return f
}

var rowRE = regexp.MustCompile(`<tr id="w-([^"]+)" class="worker-([a-z]+)">`)

// rows returns the table's worker ids and states, in order.
func rows(body string) (ids, states []string) {
	for _, m := range rowRE.FindAllStringSubmatch(body, -1) {
		ids = append(ids, m[1])
		states = append(states, m[2])
	}
	return ids, states
}

// TestWorkersTable: one row per worker, pending first, with the columns
// filled from facts, grants and the last heartbeat.
func TestWorkersTable(t *testing.T) {
	t.Parallel()
	f := newWorkerFleet(t)
	require.NoError(t, f.st.TouchWorker(f.ctx, f.active, &api.Gauges{SlotsBusy: 1}))
	f.clk.Advance(90 * time.Second)
	b := f.login("root")

	resp, body := b.get("/ui/workers")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	ids, states := rows(body)
	assert.Equal(t, []string{f.pending, f.active, f.paused, f.rejected}, ids)
	assert.Equal(t, []string{"pending", "active", "paused", "rejected"}, states)

	row := rowOf(t, body, f.active)
	assert.Contains(t, row, `<a href="/ui/workers/`+f.active+`" title="`+f.active+`">active-host</a>`,
		"no description: named by hostname")
	assert.Contains(t, row, `<span class="badge state-active">active</span>`)
	assert.Contains(t, row, `<span class="chip">public</span>`)
	assert.Contains(t, row, `<span class="badge sandbox-full">full</span>`)
	assert.Contains(t, row, ">1/1</td>", "busy/total from the gauges")
	assert.Contains(t, row, "1m 30s ago")
	assert.Contains(t, row, ">Pause</button>")
	assert.NotContains(t, row, "Resume")

	row = rowOf(t, body, f.pending)
	assert.Contains(t, row, ">–/1</td>", "no heartbeat yet")
	assert.Contains(t, row, "never")
	assert.Contains(t, row, `href="/ui/workers/`+f.pending+`#approve">Approve</a>`)
	assert.Contains(t, rowOf(t, body, f.paused), ">Resume</button>")
	assert.NotContains(t, rowOf(t, body, f.rejected), "<button")
	assert.NotContains(t, body, "Reject", "reject is on the worker's page only")
}

// rowOf returns the table row of worker id.
func rowOf(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `<tr id="w-`+id+`"`)
	require.GreaterOrEqual(t, start, 0, "no row for %s", id)
	end := strings.Index(body[start:], "</tr>")
	require.Greater(t, end, 0)
	return body[start : start+end]
}

// TestWorkersTableNaming: description, else hostname, else short id.
func TestWorkersTableNaming(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	described, _ := e.worker("box-1", "the garage box")
	b := e.login("root")
	_, body := b.get("/ui/workers")
	assert.Contains(t, rowOf(t, body, described), `>the garage box</a><div class="muted small">box-1</div>`)

	// Registration requires a hostname; the id is the last resort.
	const id = "0123456789abcdef-0123"
	assert.Equal(t, "box-1", workerName(&store.Worker{ID: id, Facts: api.WorkerFacts{Hostname: "box-1", Description: " "}}))
	assert.Equal(t, shortID(id), workerName(&store.Worker{ID: id}))
}

func TestWorkersTableStateFilter(t *testing.T) {
	t.Parallel()
	f := newWorkerFleet(t)
	b := f.login("root")
	for state, id := range map[string]string{"pending": f.pending, "active": f.active, "paused": f.paused,
		"rejected": f.rejected} {
		resp, body := b.get("/ui/workers?state=" + state)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		ids, _ := rows(body)
		assert.Equal(t, []string{id}, ids, state)
		assert.Contains(t, body, `<option value="`+state+`" selected>`, state)
	}
	require.NoError(t, f.srv.Ops().SetWorkerState(f.ctx, server.CLIActor(), f.paused, api.WorkerRejected))
	_, body := b.get("/ui/workers?state=paused")
	assert.Contains(t, body, "No workers in state paused.")

	resp, _ := b.get("/ui/workers?state=bogus")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// The filter narrows what the viewer may see; it never widens it.
	resp, body = f.login("wa").get("/ui/workers?state=pending")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	ids, _ := rows(body)
	assert.Empty(t, ids)
}

// TestWorkersTableActions: pause and resume from a row answer htmx with the
// refreshed row, fall back to a redirect without htmx, and need CSRF.
func TestWorkersTableActions(t *testing.T) {
	t.Parallel()
	f := newWorkerFleet(t)
	b := f.login("wa")
	hx := map[string]string{"HX-Request": "true"}
	rowForm := func(extra url.Values) url.Values {
		v := url.Values{"csrf": {b.csrf}, "fragment": {"row"}, "back": {"/ui/workers"}}
		for k, vs := range extra {
			v[k] = vs
		}
		return v
	}

	// No CSRF: refused, nothing changes.
	resp, _ := b.do("POST", "/ui/workers/"+f.active+"/pause", url.Values{"fragment": {"row"}}, hx)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp, _ = b.do("POST", "/ui/workers/"+f.active+"/pause", rowForm(url.Values{"csrf": {"wrong"}}), hx)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, api.WorkerActive, workerState(t, f.env, f.active))

	// htmx: the refreshed row alone.
	resp, body := b.do("POST", "/ui/workers/"+f.active+"/pause", rowForm(nil), hx)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.True(t, strings.HasPrefix(strings.TrimSpace(body), `<tr id="w-`+f.active+`" class="worker-paused">`), body)
	assert.Contains(t, body, ">Resume</button>")
	assert.Contains(t, body, `name="csrf" value="`+b.csrf+`"`, "the row's forms keep working")
	assert.NotContains(t, body, "<html")
	assert.Equal(t, api.WorkerPaused, workerState(t, f.env, f.active))

	// Without htmx: back to the list.
	resp, _ = b.post("/ui/workers/"+f.active+"/resume", url.Values{"fragment": {"row"}, "back": {"/ui/workers"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/workers", resp.Header.Get("Location"))
	assert.Equal(t, api.WorkerActive, workerState(t, f.env, f.active))

	// back is checked like next.
	resp, _ = b.post("/ui/workers/"+f.active+"/pause", url.Values{"back": {"https://evil.example/"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/", resp.Header.Get("Location"))

	// Out of scope: refused from the table as from anywhere.
	resp, _ = b.do("POST", "/ui/workers/"+f.paused+"/resume", rowForm(nil), hx)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "#flash", resp.Header.Get("HX-Retarget"))
	assert.Equal(t, api.WorkerPaused, workerState(t, f.env, f.paused))
}

// TestWorkerDetail: the page shows every fact, the controls the viewer may
// use, and the extension-point sections render nothing yet.
func TestWorkerDetail(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.queue("other")
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	status, b0 := e.api("POST", "/worker", "", api.WorkerFacts{Hostname: "gpu-1", Description: "the big one",
		CPUModel: "Ryzen 9", Cores: 16, MemBytes: 64 << 30, OS: "linux", Arch: "amd64",
		EngineVersion: "whisper.cpp 1.9.5", FFmpegVersion: "7.1", SandboxLevel: api.SandboxFSOnly,
		Kinds: []api.TaskKind{api.KindPrepare, api.KindTranscribe}, Slots: 2, Models: []string{"small.en", "large-v3"},
		Version: "1.2.3"})
	require.Equal(t, http.StatusCreated, status, string(b0))
	var reg api.RegisterResponse
	require.NoError(t, json.Unmarshal(b0, &reg))
	id := reg.ID
	b := e.login("root")

	resp, body := b.get("/ui/workers/" + id)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	for _, s := range []string{"<title>Worker the big one", `aria-current="page">Workers`, "gpu-1", "Ryzen 9", "16 cores",
		"64.0 GiB", "linux/amd64", "whisper-network 1.2.3", "engine whisper.cpp 1.9.5", "ffmpeg 7.1",
		`sandbox-fs-only">fs-only`, "prepare, transcribe", "small.en, large-v3", "never", "2026-11-15 12:00:00Z",
		`id="approve"`, `value="public"`, `value="other"`, ">Reject</button>", "hx-confirm="} {
		assert.Contains(t, body, s)
	}
	assert.NotContains(t, body, ">Pause</button>")
	assert.NotContains(t, body, "Current leases", "extension point is empty")

	// Approve from the page: back to the page, now with pause and the grant controls.
	resp, _ = b.post("/ui/workers/"+id+"/approve", url.Values{"queue": {"public"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/workers/"+id, resp.Header.Get("Location"))
	_, body = b.get("/ui/workers/" + id)
	assert.Contains(t, body, "Decided by")
	assert.Contains(t, body, "user:root")
	assert.Contains(t, body, ">Pause</button>")
	assert.Contains(t, body, `aria-label="Revoke public"`)
	assert.Contains(t, body, `<option value="queue:other">`)
	assert.NotContains(t, body, `id="approve"`)

	// htmx actions from the page navigate back to it.
	resp, _ = b.do("POST", "/ui/workers/"+id+"/reject", url.Values{"csrf": {b.csrf}},
		map[string]string{"HX-Request": "true"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "/ui/workers/"+id, resp.Header.Get("HX-Redirect"))
	assert.Equal(t, api.WorkerRejected, workerState(t, e, id))

	// CSRF is required from the page too.
	resp, _ = b.do("POST", "/ui/workers/"+id+"/pause", url.Values{}, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// TestWorkersPageStats: the workers table summarises each worker's counters
// (with the breakdown on hover) and the worker's page details them.
func TestWorkersPageStats(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	client := e.client(api.Grant{Role: api.RoleSubmit, Scope: auth.QueueScope("public")})
	e.submitAudio(client, "public", "talk", 36)
	wid, wtok := e.worker("runner", "")
	require.NoError(t, e.srv.Ops().ApproveWorker(e.ctx, server.CLIActor(), wid, []string{"public"}))
	idle, _ := e.worker("idle-host", "")
	e.runJob(wid, wtok)
	b := e.login("root")
	collapse := regexp.MustCompile(`\s+`)

	resp, body := b.get("/ui/workers")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body = collapse.ReplaceAllString(body, " ")
	for _, want := range []string{
		`<th scope="col" class="n">Tasks done</th>`, `<th scope="col" class="n">Audio hours</th>`,
		`>Avg RTF</th>`, `>Failure rate</th>`,
	} {
		assert.Contains(t, body, want)
	}
	// runner: one 36 s transcribe task in 1 s of compute.
	row := rowOf(t, body, wid)
	for _, want := range []string{
		`title="0 prepare · 1 transcribe · 1 jobs">1</td>`,
		`<td class="n">0.01</td>`,
		`<td class="n">36.0×</td>`,
		`title="0 failed · 0 expired">0%</td>`,
	} {
		assert.Contains(t, row, want)
	}
	// idle-host has done nothing: no rate to show.
	row = rowOf(t, body, idle)
	assert.Contains(t, row, `<td class="n">—</td>`)
	assert.Contains(t, row, `title="0 failed · 0 expired">—</td>`)

	// The worker's page has the full breakdown.
	resp, body = b.get("/ui/workers/" + wid)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body = collapse.ReplaceAllString(body, " ")
	for _, want := range []string{
		`<h2>Statistics</h2>`,
		`<dt>Tasks done</dt><dd>1 (0 prepare, 1 transcribe) in 1 jobs</dd>`,
		`<dt>Audio</dt><dd>0.01 h</dd>`,
		`<dt>Compute</dt><dd>1s</dd>`,
		`<dt>Realtime factor</dt><dd>36.0× realtime</dd>`,
		`<dt>Failures</dt><dd>0 failed, 0 expired · 0% failure rate</dd>`,
	} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, `<dt>Last task</dt><dd><span class="muted">never`)

	_, body = b.get("/ui/workers/" + idle)
	body = collapse.ReplaceAllString(body, " ")
	assert.Contains(t, body, `<dt>Realtime factor</dt><dd><span class="muted">none yet</span></dd>`)
	assert.Contains(t, body, `<dt>Failures</dt><dd>0 failed, 0 expired</dd>`)
	assert.Contains(t, body, `<dt>Last task</dt><dd><span class="muted">never</span></dd>`)
}
