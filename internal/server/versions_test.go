package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/store"
)

func withVersion(v string) harnessOpt { return func(c *Config, _ *store.Config) { c.Version = v } }

// The server reports its own version to whoever may list workers, and in
// whisper_server_info; workers' versions label whisper_worker_info.
func TestServerVersionReported(t *testing.T) {
	h := newHarness(t, withVersion("v0.5.0-dev+7.g420d428"))
	var v api.ServerVersion
	h.expect("GET", "/admin/version", h.admin, nil, http.StatusOK, &v)
	assert.Equal(t, "v0.5.0-dev+7.g420d428", v.Version)
	wadmin := h.client(api.Grant{Role: api.RoleWorkerAdmin, Scope: "*"})
	h.expect("GET", "/admin/version", wadmin, nil, http.StatusOK, &v)

	wid, _ := h.activeWorker(api.WorkerFacts{Hostname: "nuc", Version: "v0.4.0", EngineVersion: "1.7.4",
		FFmpegVersion: "7.1"}, "public")
	h.srv.refreshStats(h.ctx)
	var mb bytes.Buffer
	h.metrics.WritePrometheus(&mb)
	assert.Contains(t, mb.String(), `whisper_server_info{version="v0.5.0-dev+7.g420d428"} 1`)
	assert.Contains(t, mb.String(), `whisper_worker_info{worker="`+wid+`",hostname="nuc",description="nuc",version="v0.4.0",engine_version="1.7.4",ffmpeg_version="7.1",`)
}

func TestServerVersionDefaultsToUnknown(t *testing.T) {
	h := newHarness(t)
	var v api.ServerVersion
	h.expect("GET", "/admin/version", h.admin, nil, http.StatusOK, &v)
	assert.Equal(t, "unknown", v.Version)
}

// A worker restarted on another build PATCHes its facts; the audit entry
// carries the new versions and the old ones that changed.
func TestUpdateFactsAuditsVersionChange(t *testing.T) {
	h := newHarness(t)
	facts := api.WorkerFacts{Hostname: "nuc", SandboxLevel: api.SandboxFull, Version: "v0.4.0", EngineVersion: "1.7.4", FFmpegVersion: "7.1"}
	wid, wtok := h.activeWorker(facts, "public")
	wname := "worker:" + wid

	facts.Version, facts.EngineVersion = "v0.5.0", "1.7.5"
	es := h.auditStep([]string{"worker.update_facts " + wname + " " + wname}, func() {
		h.expect("PATCH", "/worker/"+wid, wtok, facts, http.StatusOK, nil)
	})
	var d map[string]any
	require.NoError(t, json.Unmarshal([]byte(es[0].Detail), &d))
	assert.Equal(t, "v0.5.0", d["version"])
	assert.Equal(t, "v0.4.0", d["old_version"])
	assert.Equal(t, "1.7.5", d["engine_version"])
	assert.Equal(t, "1.7.4", d["old_engine_version"])
	assert.Equal(t, "7.1", d["ffmpeg_version"])
	assert.NotContains(t, d, "old_ffmpeg_version", "unchanged")

	// An update that changes no version carries no old_ keys.
	facts.Slots = 3
	es = h.auditStep([]string{"worker.update_facts " + wname + " " + wname}, func() {
		h.expect("PATCH", "/worker/"+wid, wtok, facts, http.StatusOK, nil)
	})
	d = nil
	require.NoError(t, json.Unmarshal([]byte(es[0].Detail), &d))
	for _, k := range []string{"old_version", "old_engine_version", "old_ffmpeg_version"} {
		assert.NotContains(t, d, k)
	}
	wk, err := h.st.WorkerByID(h.ctx, wid)
	require.NoError(t, err)
	assert.Equal(t, "v0.5.0", wk.Facts.Version)
}
