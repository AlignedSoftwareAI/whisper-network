package ui

import (
	"encoding/json"
	"html"
	"io/fs"
	"net/http"
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

// storeLabelled creates a one-task canonical job on public with a label
// and source.
func (e *env) storeLabelled(owner, label, source string) *store.Job {
	e.t.Helper()
	q, err := e.st.QueueByName(e.ctx, "public")
	require.NoError(e.t, err)
	j, _, err := e.st.CreateJob(e.ctx, store.NewJob{QueueID: q.ID, ClientID: owner,
		Params:    api.JobParams{Model: "small.en", Label: label, Source: source},
		Original:  store.Media{Bytes: 100, SHA256: "o", Type: "audio/flac"},
		Canonical: &store.Canonical{Bytes: 100, SHA256: "c", Samples: 16000},
		Ranges:    []store.SampleRange{{Start: 0, End: 16000}}})
	require.NoError(e.t, err)
	return j
}

// registerWorker registers a worker with facts, approves it for public and
// reports gauges (which also marks it seen).
func (e *env) registerWorker(facts api.WorkerFacts, g *api.Gauges) (string, string) {
	e.t.Helper()
	status, b := e.api("POST", "/worker", "", facts)
	require.Equal(e.t, http.StatusCreated, status, string(b))
	var reg api.RegisterResponse
	require.NoError(e.t, json.Unmarshal(b, &reg))
	require.NoError(e.t, e.srv.Ops().ApproveWorker(e.ctx, server.CLIActor(), reg.ID, []string{"public"}))
	if g != nil {
		status, b = e.api("POST", "/worker/"+reg.ID+"/gauges", reg.Token, g)
		require.Equal(e.t, http.StatusNoContent, status, string(b))
	}
	return reg.ID, reg.Token
}

var (
	thRE   = regexp.MustCompile(`(?s)<th\b([^>]*)>(.*?)</th>`)
	dtRE   = regexp.MustCompile(`(?s)<dt\b([^>]*)>(.*?)</dt>`)
	descRE = regexp.MustCompile(`aria-describedby="([^"]+)"`)

	helpRE      = regexp.MustCompile(`<button type="button" class="colhelp"[^>]*>ⓘ</button><span class="colhelp-text"[^>]*>.*?</span>`)
	dtTitleRE   = regexp.MustCompile(`<dt title="[^"]*">`)
	tileLabelRE = regexp.MustCompile(`<span class="tile-label" title="[^"]*">`)
)

// plainHelp strips field and tile help from a page, so that tests about a
// field's value can match "<dt>Label</dt><dd>…" and "<span>caption</span>".
func plainHelp(body string) string {
	body = helpRE.ReplaceAllString(body, "")
	body = dtTitleRE.ReplaceAllString(body, "<dt>")
	return tileLabelRE.ReplaceAllString(body, "<span>")
}

// TestColumnHelp walks every page and fails on a column header, a
// definition-list term or a dashboard tile without help (#79): a tooltip,
// and an ⓘ button whose aria-describedby names the help text on the page.
// Row headers (the grant matrix's queue names) are not columns. The pages
// are set up to show every conditional field they can; TestTemplatesUseHelp
// covers the rest statically.
func TestColumnHelp(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	wa, waTok := e.approved()
	failed := e.storeJob("someone", 1)
	e.failJob(wa, waTok)
	j := e.storeJob("someone", 2)
	e.claim(wa, waTok)
	free, load := int64(1<<30), 50.0
	wg, _ := e.registerWorker(api.WorkerFacts{Hostname: "g", OS: "linux", Arch: "amd64", Cores: 4,
		SandboxLevel: api.SandboxFull, Kinds: []api.TaskKind{api.KindTranscribe}, Slots: 1, Models: []string{"small.en"}},
		&api.Gauges{ModelsBytes: &free, WorkBytes: &free, StateFreeBytes: &free, HostLoad: &load, DeferredForLoad: true,
			CachedModels: []api.CachedModel{{Name: "small.en", Bytes: 1}}})
	clients, err := e.st.ListClients(e.ctx)
	require.NoError(t, err)

	b := e.login("root")
	_, bulk := b.post("/ui/queues/public/bulk", map[string][]string{"op": {"retry-selected"},
		"job_id": {failed.ID}, "confirm": {"1"}})
	pages := map[string]string{"bulk result": bulk}
	for _, p := range []string{"/ui/", "/ui/workers", "/ui/workers/" + wa, "/ui/workers/" + wg, "/ui/queues",
		"/ui/queues/public", "/ui/jobs/" + j.ID, "/ui/jobs/" + failed.ID,
		"/ui/users", "/ui/clients", "/ui/clients/" + clients[0].ID, "/ui/audit"} {
		resp, body := b.get(p)
		require.Equal(t, http.StatusOK, resp.StatusCode, p)
		pages[p] = body
	}
	labelled := func(p, body, tag string, attrs, inner string) {
		assert.Contains(t, attrs, `title="`, "%s: %s", p, tag)
		assert.Contains(t, inner, `<button type="button" class="colhelp"`, "%s: %s", p, tag)
		assert.Contains(t, inner, `aria-expanded="false"`, "%s: %s", p, tag)
		m := descRE.FindStringSubmatch(inner)
		if assert.NotNil(t, m, "%s: %s", p, tag) {
			assert.Equal(t, 1, strings.Count(body, `id="`+m[1]+`"`), "%s: help %s is on the page once", p, m[1])
		}
	}
	for p, body := range pages {
		headers, terms := thRE.FindAllStringSubmatch(body, -1), dtRE.FindAllStringSubmatch(body, -1)
		assert.NotEmpty(t, append(headers, terms...), p)
		for _, h := range headers {
			if strings.Contains(h[1], `scope="row"`) {
				continue
			}
			labelled(p, body, h[0], h[1], h[2])
		}
		for _, d := range terms {
			labelled(p, body, d[0], d[1], d[2])
		}
		// A tile holds no nested div, so it ends at the first </div>.
		for _, tile := range strings.Split(body, `<div class="tile`)[1:] {
			tile, _, _ = strings.Cut(tile, "</div>")
			assert.Contains(t, tile, `<span class="tile-label" title="`, "%s: tile %s", p, tile)
			assert.Contains(t, tile, `<button type="button" class="colhelp"`, "%s: tile %s", p, tile)
		}
	}
	for _, p := range []string{"/ui/workers/" + wa, "/ui/workers/" + wg, "/ui/jobs/" + failed.ID} {
		assert.NotEmpty(t, dtRE.FindAllString(pages[p], -1), p)
	}
	assert.Contains(t, pages["/ui/workers/"+wg], `id="help-workerfact-hostcpu"`, "conditional facts are walked")
	assert.Contains(t, pages["/ui/workers/"+wg], `id="help-workerfact-cached"`)
	assert.Contains(t, pages["/ui/jobs/"+failed.ID], `id="help-jobfact-retries"`)
	assert.Equal(t, 6, strings.Count(pages["/ui/"], `<span class="tile-label"`), "every dashboard tile is walked")
	assert.Contains(t, pages["/ui/audit"], `as &lt;area&gt;.&lt;verb&gt;`, "help text is escaped")
	assert.Contains(t, pages["/ui/workers"], `Average realtime factor: seconds of audio transcribed per second of compute.`)
	assert.Contains(t, pages["/ui/clients/"+clients[0].ID], `aria-describedby="help-role-submit"`,
		"the grant matrix's role columns explain themselves")

	resp, js := b.get(regexp.MustCompile(`src="(/ui/static/ui\.js\?v=[0-9a-f]+)"`).FindStringSubmatch(pages["/ui/"])[1])
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, js, "aria-expanded")
}

// TestTemplatesUseHelp checks the templates themselves, so that a field shown
// only in a state no test reaches still has help: no page writes a bare
// <dt>, a tile without a tile-label, or a help key the map lacks.
func TestTemplatesUseHelp(t *testing.T) {
	t.Parallel()
	keyRE := regexp.MustCompile(`\(col "([^"]+)"`)
	files, err := fs.Glob(templateFS, "templates/*.html")
	require.NoError(t, err)
	for _, f := range files {
		b, err := fs.ReadFile(templateFS, f)
		require.NoError(t, err)
		src := string(b)
		if f != "templates/layout.html" {
			assert.NotContains(t, src, "<dt", "%s: use {{template \"dt\" (col …)}}", f)
		}
		for _, tile := range strings.Split(src, `<div class="tile`)[1:] {
			tile, _, _ = strings.Cut(tile, "</div>")
			assert.Contains(t, tile, `{{template "tile-label" (col "`, "%s: tile %s", f, tile)
		}
		for _, m := range keyRE.FindAllStringSubmatch(src, -1) {
			_, err := col(m[1])
			assert.NoError(t, err, f)
		}
	}
}

func TestColumnLookup(t *testing.T) {
	t.Parallel()
	c, err := col("workers.rtf", "n")
	require.NoError(t, err)
	assert.Equal(t, "help-workers-rtf", c.ID)
	assert.Equal(t, "n", c.Class)
	_, err = col("workers.nope")
	assert.Error(t, err, "an unexplained column fails the render")
}

// rc builds a request context for a user principal with grants.
func rcWith(grants ...api.Grant) *reqCtx {
	return &reqCtx{P: &server.Principal{Principal: auth.Principal{Kind: auth.KindUser, ID: "9", Grants: grants}}}
}

// TestLabeler: refs name, link, mark and hide per the visibility rules
// (#57, #58, #60).
func TestLabeler(t *testing.T) {
	t.Parallel()
	admin := rcWith(api.Grant{Role: api.RoleAdmin, Scope: "*"})
	viewer := rcWith(api.Grant{Role: api.RoleViewAll, Scope: auth.QueueScope("public")})
	maps := func(rc *reqCtx) *labeler {
		return &labeler{rc: rc,
			workers: map[string]store.WorkerRef{
				"w-1234567890ab": {ID: "w-1234567890ab", Hostname: "tc-3"},
				"w-anonymous-00": {ID: "w-anonymous-00"},
			},
			jobs: map[string]store.JobRef{
				"0000-job-1234": {ID: "0000-job-1234", Label: "weekly planning meeting", QueueName: "public", ClientID: "c"},
				"0000-job-5678": {ID: "0000-job-5678", Source: "https://example.org/a.mp3", QueueName: "public", ClientID: "c"},
				"0000-job-9999": {ID: "0000-job-9999", QueueName: "public", ClientID: "c"},
				"0000-job-long": {ID: "0000-job-long", Label: strings.Repeat("x", 70), QueueName: "public", ClientID: "c"},
				"0000-job-priv": {ID: "0000-job-priv", Label: "secret", QueueName: "private", ClientID: "c"},
			},
			clients: map[string]store.ClientRef{
				"cli_8a6a5685ffff": {ID: "cli_8a6a5685ffff", Name: "transcriber"},
				"cli_revokedxxxxx": {ID: "cli_revokedxxxxx", Name: "old", Revoked: true},
			},
		}
	}
	a, v := maps(admin), maps(viewer)

	assert.Equal(t, ref{Name: "tc-3", Short: "w-123456…90ab", Title: "w-1234567890ab", Href: "/ui/workers/w-1234567890ab"},
		a.worker("w-1234567890ab", ""))
	assert.Equal(t, ref{Short: "w-anonym…s-00", Title: "w-anonymous-00", Href: "/ui/workers/w-anonymous-00"},
		a.worker("w-anonymous-00", "public"), "no name: the id alone")
	assert.Equal(t, ref{Short: "w-pruned…0000", Title: "w-pruned-00000000", Note: "removed"},
		a.worker("w-pruned-00000000", ""), "a pruned worker is unlinked")
	assert.Equal(t, ref{Name: "a worker", Muted: true}, v.worker("w-1234567890ab", "public"))

	assert.Equal(t, ref{Name: "weekly planning meeting", Short: "…1234", Title: "0000-job-1234", Href: "/ui/jobs/0000-job-1234"},
		v.job("0000-job-1234"))
	assert.Equal(t, "https://example.org/a.mp3", v.job("0000-job-5678").Name, "no label: the source")
	assert.Equal(t, ref{Short: "…9999", Title: "0000-job-9999", Href: "/ui/jobs/0000-job-9999"}, v.job("0000-job-9999"))
	long := v.job("0000-job-long")
	assert.Equal(t, strings.Repeat("x", 59)+"…", long.Name)
	assert.Equal(t, strings.Repeat("x", 70)+" · 0000-job-long", long.Title)
	assert.Equal(t, ref{Short: "…priv", Title: "0000-job-priv"}, v.job("0000-job-priv"), "invisible: no label, no link")
	assert.Equal(t, ref{Short: "…gone", Title: "0000-job-gone", Note: "deleted"}, a.job("0000-job-gone"))

	assert.Equal(t, ref{Name: "transcriber", Short: "cli_8a6a…", Title: "cli_8a6a5685ffff", Href: "/ui/clients/cli_8a6a5685ffff"},
		a.principal("client:cli_8a6a5685ffff"))
	assert.Equal(t, "revoked", a.client("cli_revokedxxxxx").Note)
	assert.Equal(t, ref{Name: "a client", Muted: true}, v.client("cli_8a6a5685ffff"))
	assert.Equal(t, ref{Name: "transcriber", Short: "cli_8a6a…", Title: "cli_8a6a5685ffff"}, v.submitter("cli_8a6a5685ffff"),
		"a job's submitter is named to its viewers (#82), unlinked without client_admin")
	assert.Equal(t, "/ui/clients/cli_8a6a5685ffff", a.submitter("cli_8a6a5685ffff").Href)
	assert.Equal(t, ref{Name: "alice", Note: "user"}, a.principal("user:alice"))
	assert.Equal(t, ref{Name: "public", Note: "queue", Href: "/ui/queues/public"}, a.principal("queue:public"))
	assert.Equal(t, ref{Name: "system"}, a.principal("system"))

	reason := "transcribe task 1 failed after 2 attempt(s) (on workers w-1234567890ab, w-anonymous-00): boom"
	segs := a.withWorkers(reason, "public", []string{"w-1234567890ab", "w-anonymous-00"})
	require.Len(t, segs, 5)
	assert.Equal(t, "tc-3", segs[1].Ref.Name)
	assert.Equal(t, ", ", segs[2].Text)
	assert.Equal(t, "): boom", segs[4].Text)
	for _, s := range v.withWorkers(reason, "public", []string{"w-1234567890ab", "w-anonymous-00"}) {
		assert.NotContains(t, s.Text, "w-1234567890ab", "viewers do not see worker ids")
		if s.Ref != nil {
			assert.Equal(t, "a worker", s.Ref.Name)
		}
	}
}

// TestLabelsRendered: jobs, workers and clients appear by name and link to
// their pages, escaped, and hidden from viewers who may not see them.
func TestLabelsRendered(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	e.user("viewer", api.Grant{Role: api.RoleViewAll, Scope: auth.QueueScope("public")})
	adminClient, err := e.st.ListClients(e.ctx)
	require.NoError(t, err)
	owner := adminClient[0].ID
	labelled := e.storeLabelled(owner, "weekly planning meeting", "")
	sourced := e.storeLabelled(owner, "", "https://example.org/council.mp3")
	plain := e.storeLabelled(owner, "", "")
	xss := e.storeLabelled(owner, `<script>alert("x")</script>`, "")
	wid, wtok := e.worker("box-a", `<b>alpha</b>`)
	require.NoError(t, e.srv.Ops().ApproveWorker(e.ctx, server.CLIActor(), wid, []string{"public"}))
	l := e.claim(wid, wtok)
	require.Equal(t, labelled.ID, l.Job)

	root, viewer := e.login("root"), e.login("viewer")
	_, body := root.get("/ui/queues/public")
	assert.Contains(t, body, `<a class="ref" href="/ui/jobs/`+labelled.ID+`" title="`+labelled.ID+
		`">weekly planning meeting <span class="muted">(`+jobShort(labelled.ID)+`)</span></a>`)
	assert.Contains(t, body, `>https://example.org/council.mp3 <span class="muted">(`)
	assert.Contains(t, body, `href="/ui/jobs/`+plain.ID+`" title="`+plain.ID+`"><code>`+jobShort(plain.ID)+`</code></a>`)
	assert.Contains(t, body, `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;`)
	assert.NotContains(t, body, `<script>alert`)
	assert.NotContains(t, body, `>Label<button`, "the label is merged into the Job column")
	assert.Contains(t, body, `&lt;b&gt;alpha&lt;/b&gt; <span class="muted">(`+shortID(wid)+`)</span>`, "worker named, escaped")
	_ = sourced
	_ = xss

	_, body = root.get("/ui/jobs/" + labelled.ID)
	assert.Contains(t, body, `<h1>Job weekly planning meeting <span class="muted small"><code>`+labelled.ID+`</code></span></h1>`)
	assert.Contains(t, body, `href="/ui/clients/`+owner+`"`, "root sees the submitting client, linked")

	_, body = viewer.get("/ui/jobs/" + labelled.ID)
	body = plainHelp(body)
	assert.Contains(t, body, `<dt>Submitted by</dt><dd><span class="ref" title="`+owner+`">`+html.EscapeString(adminClient[0].Name)+` <span class="muted">(`,
		"a viewer of the job sees which client submitted it (#82)")
	assert.NotContains(t, body, `a client</span>`, "a viewer of the job sees who submitted it (#82)")
	assert.NotContains(t, body, "/ui/clients/", "but no link to a client page it may not open")

	_, body = root.get("/ui/workers/" + wid)
	assert.Contains(t, body, `>weekly planning meeting <span class="muted">(`, "current leases by label")

	// Audit: the admin client deleting a job, and the worker's approval.
	status, b := e.api("DELETE", "/job/"+plain.ID, e.admin, nil)
	require.Equal(t, http.StatusNoContent, status, string(b))
	_, body = root.get("/ui/audit")
	assert.Contains(t, body, `<a class="ref" href="/ui/clients/`+owner+`" title="`+owner+`">test <span class="muted">(`+
		clientShort(owner)+`)</span></a>`, "client actor")
	assert.Contains(t, body, `<span class="ref" title="`+plain.ID+`"><code>`+jobShort(plain.ID)+
		`</code> <span class="muted">(deleted)</span></span>`, "a deleted job target")
	assert.Contains(t, body, `href="/ui/workers/`+wid+`"`, "worker target")

	_, body = root.get("/ui/clients/" + owner)
	assert.Contains(t, body, "Recent activity")
	assert.Contains(t, body, "<td>job.delete</td>")
	assert.Contains(t, body, "Save permissions")
	_, body = root.get("/ui/clients")
	assert.Contains(t, body, `<a href="/ui/clients/`+owner+`">test</a>`)
}

// TestRevokedClientPage: a revoked client's page still shows its details,
// marked, without the permissions form.
func TestRevokedClientPage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	c, err := e.srv.Ops().CreateClient(e.ctx, server.CLIActor(), "retired", nil)
	require.NoError(t, err)
	require.NoError(t, e.srv.Ops().RevokeClient(e.ctx, server.CLIActor(), c.ID))
	resp, body := e.login("root").get("/ui/clients/" + c.ID)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	body = plainHelp(body)
	assert.Contains(t, body, `<h1>Client retired <span class="badge state-rejected"`)
	assert.Contains(t, body, "<dt>Revoked</dt>")
	assert.Contains(t, body, `by <span class="ref">system</span>`)
	assert.NotContains(t, body, "Save permissions")
}

// TestFailureReasonNamesWorkers: a failed job's error names the worker by
// name for those who may see it, and hides its id from others (#57).
func TestFailureReasonNamesWorkers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	e.user("viewer", api.Grant{Role: api.RoleViewAll, Scope: auth.QueueScope("public")})
	wid, wtok := e.approved()
	e.storeJob("someone", 1)
	id := e.failJob(wid, wtok)

	_, body := e.login("root").get("/ui/jobs/" + id)
	assert.Regexp(t, `<dd class="error">transcribe task 1 failed after 1 attempt\(s\) \(on worker <a class="ref" href="/ui/workers/`+
		wid+`" title="`+wid+`">runner <span class="muted">\(`+regexp.QuoteMeta(shortID(wid))+`\)</span></a>\): bad input; `, body)

	_, body = e.login("viewer").get("/ui/jobs/" + id)
	assert.Contains(t, body, `(on worker <span class="ref muted">a worker</span>): bad input;`)
	assert.NotContains(t, body, wid)
	assert.NotContains(t, body, "runner")
}

// TestCapacityTile: the dashboard's Capacity tile and per-queue eligible
// slots (#81).
func TestCapacityTile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	both := []api.TaskKind{api.KindPrepare, api.KindTranscribe}
	facts := func(slots int, kinds []api.TaskKind) api.WorkerFacts {
		return api.WorkerFacts{Hostname: "h", OS: "linux", Arch: "amd64", Cores: 4, SandboxLevel: api.SandboxFull,
			Kinds: kinds, Slots: slots, Models: []string{"small.en"}}
	}
	e.registerWorker(facts(4, both), &api.Gauges{SlotsBusy: 3})
	e.registerWorker(facts(2, []api.TaskKind{api.KindTranscribe}), &api.Gauges{DeferredForLoad: true})
	paused, _ := e.registerWorker(facts(3, both), &api.Gauges{})
	require.NoError(t, e.srv.Ops().SetWorkerState(e.ctx, server.CLIActor(), paused, api.WorkerPaused))
	e.storeJob("someone", 1)

	_, body := e.login("root").get("/ui/")
	body = plainHelp(regexp.MustCompile(`\s+`).ReplaceAllString(body, " "))
	for _, want := range []string{
		`<span class="big">3/6</span><span>slots in use · capacity</span>`,
		`<small class="available">1 available · 2 deferred</small>`,
		`<small class="by-kind">transcribe 3/6 · prepare 3/4</small>`,
		`<small class="apart">+3 slots paused</small>`,
		`<td class="n slots">3/6</td>`,
	} {
		assert.Contains(t, body, want)
	}
}

func TestWaitingReasons(t *testing.T) {
	t.Parallel()
	worker := func(id string, slots int, kinds []api.TaskKind, sandbox api.SandboxLevel, models ...string) store.Worker {
		return store.Worker{ID: id, Facts: api.WorkerFacts{Slots: slots, Kinds: kinds, SandboxLevel: sandbox, Models: models}}
	}
	tr := []api.TaskKind{api.KindTranscribe}
	fleet := func(ws []store.Worker, gs ...*api.Gauges) *fleetView {
		f := &fleetView{queue: "public", workers: ws, gauges: make([]*api.Gauges, len(ws)), prepareMin: api.SandboxFull}
		copy(f.gauges, gs)
		return f
	}
	one := func(t *testing.T, got []string, want string) {
		t.Helper()
		require.Len(t, got, 1)
		assert.Contains(t, got[0], want)
	}

	one(t, fleet(nil).waitingReasons(api.KindTranscribe, "small.en", nil), "No active worker may process queue public")
	small := []store.Worker{worker("a", 1, tr, api.SandboxFull, "small.en")}
	one(t, fleet(small).waitingReasons(api.KindTranscribe, "large-v3", nil), "No eligible worker offers model large-v3")
	one(t, fleet(small).waitingReasons(api.KindPrepare, "small.en", nil), "sandbox level full")
	fsOnly := []store.Worker{worker("p", 1, []api.TaskKind{api.KindPrepare}, api.SandboxFSOnly)}
	one(t, fleet(fsOnly).waitingReasons(api.KindPrepare, "small.en", nil), "sandbox level full")
	one(t, fleet(small).waitingReasons(api.KindTranscribe, "small.en", []string{"a"}), "The only eligible worker able to run it already failed it")
	two := append(small, worker("b", 1, tr, api.SandboxFull, "small.en"))
	one(t, fleet(two).waitingReasons(api.KindTranscribe, "small.en", []string{"a", "b"}), "All 2 eligible workers")
	one(t, fleet(two, &api.Gauges{SlotsBusy: 1}, &api.Gauges{SlotsBusy: 1}).waitingReasons(api.KindTranscribe, "small.en", nil),
		"Workers are busy")
	got := fleet(two, &api.Gauges{SlotsBusy: 1}, &api.Gauges{DeferredForLoad: true}).waitingReasons(api.KindTranscribe, "small.en", nil)
	require.Len(t, got, 2)
	assert.Contains(t, got[1], "host is busy with other work")
	assert.Empty(t, fleet(two, &api.Gauges{SlotsBusy: 1}).waitingReasons(api.KindTranscribe, "small.en", nil),
		"a free slot: waiting its turn")
	one(t, fleet(two, &api.Gauges{SlotsBusy: 1}).waitingReasons(api.KindTranscribe, "small.en", []string{"b"}),
		"Workers are busy")
}

// TestPendingDetail: the queue page's pending summary and the job page's
// waiting tasks, with why they wait (#54).
func TestPendingDetail(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	e.user("viewer", api.Grant{Role: api.RoleViewAll, Scope: auth.QueueScope("public")})
	e.user("sub", api.Grant{Role: api.RoleSubmit, Scope: auth.QueueScope("public")})
	j := e.storeJob("someone", 2)
	e.clk.Advance(90 * time.Second)
	root := e.login("root")

	// Nothing may process the queue.
	_, body := root.get("/ui/queues/public")
	flat := regexp.MustCompile(`\s+`).ReplaceAllString(body, " ")
	assert.Contains(t, flat, `<strong>2</strong> pending tasks: 2 transcribe, 0 prepare · 0.00 h of audio · oldest waiting 1m 30s`)
	assert.Contains(t, flat, `Eligible workers: <strong>0</strong>`)
	assert.Contains(t, body, `class="warn no-eligible"`)
	assert.Contains(t, body, "No active worker may process queue public")
	assert.Contains(t, body, `<div class="small warn waiting">waiting 1m 30s</div>`)

	_, body = root.get("/ui/jobs/" + j.ID)
	assert.Contains(t, body, "Waiting and failed tasks")
	assert.Contains(t, body, `<td class="n">1m 30s</td>`)
	assert.Contains(t, body, "No active worker may process queue public")

	_, body = e.login("sub").get("/ui/queues/public")
	assert.NotContains(t, body, `id="pending"`, "the summary is for those who see every job")

	// A worker takes task 1 and fails it; it is the only eligible worker.
	wid, wtok := e.approved()
	l := e.claim(wid, wtok)
	require.Equal(t, 1, l.Seq)
	e.clk.Advance(time.Minute)
	status, b := e.api("POST", "/lease/"+l.ID+"/failure", wtok, api.FailureReport{Reason: "whisper crashed", Retryable: true})
	require.Equal(t, http.StatusNoContent, status, string(b))
	e.clk.Advance(30 * time.Second)

	_, body = root.get("/ui/jobs/" + j.ID)
	rows := waitingTable(t, body)
	assert.Contains(t, rows[0], `<td class="n">30s</td>`, "pending again since the failure")
	assert.Contains(t, rows[0], `whisper crashed <span class="muted">on</span> <a class="ref" href="/ui/workers/`+wid+`"`)
	assert.Contains(t, rows[0], "The only eligible worker able to run it already failed it")
	assert.Contains(t, rows[1], `<td class="n">3m 0s</td>`)
	assert.Contains(t, rows[1], "waiting its turn")

	_, body = e.login("viewer").get("/ui/jobs/" + j.ID)
	rows = waitingTable(t, body)
	assert.Contains(t, rows[0], `<span class="ref muted">a worker</span>`)
	assert.NotContains(t, body, wid, "the viewer does not see which worker failed it")

	// Its slot is busy.
	status, b = e.api("POST", "/worker/"+wid+"/gauges", wtok, api.Gauges{SlotsBusy: 1})
	require.Equal(t, http.StatusNoContent, status, string(b))
	_, body = root.get("/ui/jobs/" + j.ID)
	assert.Contains(t, waitingTable(t, body)[1], "Workers are busy")
	_, body = root.get("/ui/queues/public")
	flat = regexp.MustCompile(`\s+`).ReplaceAllString(body, " ")
	assert.Contains(t, flat, `Eligible workers: <strong>1</strong> · slots 1/1 in use`)
	assert.NotContains(t, body, `no-eligible`)
}

// waitingTable returns the rows of the job page's waiting tasks table.
func waitingTable(t *testing.T, body string) []string {
	t.Helper()
	i := strings.Index(body, `<table class="data waiting">`)
	require.GreaterOrEqual(t, i, 0, "no waiting table")
	table := body[i:]
	table = table[:strings.Index(table, "</table>")]
	parts := strings.Split(table, "<tr>")[2:] // after the header row
	for k := range parts {
		parts[k] = html.UnescapeString(parts[k])
	}
	return parts
}
