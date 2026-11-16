package ui

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/server"
)

const serverVersion = "v0.5.0-dev+7.gabc1234"

// esc is a version as html/template writes it.
func esc(v string) string { return strings.ReplaceAll(v, "+", "&#43;") }

// versionFleet has active workers on one version each: the server's, an
// older, a newer and an unorderable one.
type versionFleet struct {
	*env
	same, older, newer, differs string
	tokens                      map[string]string
}

func newVersionFleet(t *testing.T) *versionFleet {
	t.Helper()
	f := &versionFleet{env: newEnv(t, func(c *server.Config) {
		c.Version, c.RegisterBurst = serverVersion, 10
	}), tokens: map[string]string{}}
	for _, w := range []struct {
		id              *string
		version, engine string
	}{
		{&f.same, serverVersion, "1.7.4"},
		{&f.older, "v0.4.2", "1.7.4"},
		{&f.newer, "v0.5.0-dev+9.gdef5678", "1.7.4"},
		{&f.differs, "0.0.0-dev+g1234abc.dirty", "1.7.5"},
	} {
		id, tok := f.worker("host-"+w.version, "")
		require.NoError(t, f.srv.Ops().ApproveWorker(f.ctx, server.CLIActor(), id, []string{"public"}))
		f.tokens[id] = tok
		*w.id = id
		f.setVersions(id, w.version, w.engine)
	}
	f.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	return f
}

// setVersions PATCHes a worker's facts as it would on restart.
func (f *versionFleet) setVersions(id, version, engine string) {
	f.t.Helper()
	status, b := f.api("PATCH", "/worker/"+id, f.tokens[id], api.WorkerFacts{Hostname: "host-" + id,
		SandboxLevel: api.SandboxFull, Kinds: []api.TaskKind{api.KindTranscribe}, Slots: 1,
		Models: []string{"small.en"}, Version: version, EngineVersion: engine, FFmpegVersion: "7.1"})
	require.Equal(f.t, http.StatusOK, status, string(b))
}

var versionCellRE = regexp.MustCompile(`(?s)<td class="version" title="([^"]*)">(.*?)</td>`)

// versionCell returns the tooltip and contents of a worker's Version cell.
func versionCell(t *testing.T, body, id string) (title, cell string) {
	t.Helper()
	i := strings.Index(body, `<tr id="w-`+id+`"`)
	require.GreaterOrEqual(t, i, 0, "row for %s", id)
	m := versionCellRE.FindStringSubmatch(body[i:])
	require.NotNil(t, m, "version cell for %s", id)
	return m[1], m[2]
}

func TestWorkersVersionColumn(t *testing.T) {
	f := newVersionFleet(t)
	b := f.login("root")
	resp, body := b.get("/ui/workers")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, `aria-describedby="help-workers-version"`, "the Version column explains itself (#79)")

	title, cell := versionCell(t, body, f.same)
	assert.Equal(t, "engine 1.7.4 · ffmpeg 7.1", title)
	assert.Contains(t, cell, "<code>"+esc(serverVersion)+"</code>")
	for _, flag := range []string{"outdated", "newer than server", "differs"} {
		assert.NotContains(t, cell, flag, "the server's own version shows plain")
	}

	_, cell = versionCell(t, body, f.older)
	assert.Contains(t, cell, "<code>v0.4.2</code>")
	assert.Contains(t, cell, `<span class="badge outdated"`)

	_, cell = versionCell(t, body, f.newer)
	assert.Contains(t, cell, "newer than server")
	assert.NotContains(t, cell, "outdated")

	title, cell = versionCell(t, body, f.differs)
	assert.Equal(t, "engine 1.7.5 · ffmpeg 7.1", title)
	assert.Contains(t, cell, `<span class="badge differs"`)

	// The worker restarts on the server's build: the badge goes, and the
	// change is audited with the old version.
	f.setVersions(f.older, serverVersion, "1.7.4")
	_, body = b.get("/ui/workers")
	_, cell = versionCell(t, body, f.older)
	assert.Contains(t, cell, "<code>"+esc(serverVersion)+"</code>")
	assert.NotContains(t, cell, "outdated")
	_, body = b.get("/ui/audit?action=worker.update_facts")
	assert.Contains(t, body, `&#34;old_version&#34;:&#34;v0.4.2&#34;`)
}

func TestWorkersVersionUnreported(t *testing.T) {
	e := newEnv(t, func(c *server.Config) { c.Version = serverVersion })
	id, _ := e.worker("bare", "")
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	_, body := e.login("root").get("/ui/workers")
	title, cell := versionCell(t, body, id)
	assert.Equal(t, "engine unknown · ffmpeg unknown", title)
	assert.Contains(t, cell, `<span class="muted">—</span>`)
	assert.NotContains(t, cell, "badge")
}

func TestDashboardFleetVersions(t *testing.T) {
	f := newVersionFleet(t)
	// A second worker on the older version, and a pending one, which is
	// not counted.
	id, tok := f.worker("older-2", "")
	require.NoError(t, f.srv.Ops().ApproveWorker(f.ctx, server.CLIActor(), id, []string{"public"}))
	f.tokens[id] = tok
	f.setVersions(id, "v0.4.2", "1.7.4")
	f.worker("pending", "")

	_, body := f.login("root").get("/ui/dashboard/stats")
	i := strings.Index(body, `class="tile fleet-versions"`)
	require.GreaterOrEqual(t, i, 0)
	tile := body[i:]
	tile = tile[:strings.Index(tile, "</div>")]
	assert.Contains(t, tile, "server <code>"+esc(serverVersion)+"</code>")
	assert.Contains(t, tile, `<code>v0.4.2</code> ×2 <span class="badge outdated">`, "most common first")
	assert.Contains(t, tile, "<code>"+esc(serverVersion)+"</code> ×1")
	assert.Contains(t, tile, "<code>"+esc("v0.5.0-dev+9.gdef5678")+"</code> ×1 newer than server")
	assert.Contains(t, tile, "<code>"+esc("0.0.0-dev+g1234abc.dirty")+`</code> ×1 <span class="badge differs">`)
	assert.Contains(t, tile, "engine <code>1.7.4</code> ×4 · <code>1.7.5</code> ×1")
	assert.Contains(t, tile, `<span class="badge version-mix"`, "mixed engines are flagged")
	assert.NotContains(t, tile, "unknown", "the pending worker is not counted")

	// One engine version: no flag.
	f.setVersions(f.differs, "v0.4.2", "1.7.4")
	_, body = f.login("root").get("/ui/dashboard/stats")
	assert.Contains(t, body, "engine <code>1.7.4</code> ×5")
	assert.NotContains(t, body, "version-mix")
}
