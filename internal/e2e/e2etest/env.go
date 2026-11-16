// Package e2etest runs the real server and real workers together in one
// process, for the cross-component tests in internal/e2e and the CLI test in
// program. The server listens on loopback with a temporary data directory;
// workers run the real worker code with the workertest fake tools
// (whisper-cli and ffmpeg are the test binary under another name) under the
// real sandbox-exec. A package using it must call workertest.Main from its
// TestMain.
//
// Models are small fake files pinned in a test catalog (Catalog) shared by
// the server's LookupModel and the workers' Catalog, so nothing is fetched
// from the internet.
package e2etest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/blobs"
	"github.com/deweysasser/whisper-network/internal/client"
	"github.com/deweysasser/whisper-network/internal/models"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
	"github.com/deweysasser/whisper-network/internal/testscale"
	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

// Fake model files: small, with realistic memory footprints for slot sizing.
var (
	ModelBytes = bytes.Repeat([]byte("e2e-ggml-model "), 2048)
	VADBytes   = bytes.Repeat([]byte("e2e-silero "), 256)

	// Model is the whisper model every test job uses.
	Model = pin("e2e.en", models.KindWhisper, ModelBytes, 100<<20)
	// VAD is the voice-activity model, served only when a test enables it.
	VAD = pin("e2e-vad", models.KindVAD, VADBytes, 0)
)

func pin(name string, kind models.Kind, data []byte, peak int64) models.Model {
	sum := sha256.Sum256(data)
	return models.Model{Name: name, Kind: kind, File: "ggml-" + name + ".bin", SHA256: hex.EncodeToString(sum[:]),
		Size: int64(len(data)), PeakRSS: peak, CostFactor: 1}
}

// Catalog is the pinned model table of the server and the workers.
func Catalog() []models.Model { return []models.Model{Model, VAD} }

// LookupModel resolves names in Catalog.
func LookupModel(name string) (models.Model, error) {
	for _, m := range Catalog() {
		if m.Name == name {
			return m, nil
		}
	}
	return models.Model{}, fmt.Errorf("unknown model %q", name)
}

// SyncBuffer is a goroutine-safe log sink.
type SyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *SyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Bytes returns a copy of everything written so far.
func (b *SyncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// Events decodes the JSONL lines whose event field is ev ("" for all).
func (b *SyncBuffer) Events(t testing.TB, ev obs.Event) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(b.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal(line, &m), string(line))
		if ev == "" || m[obs.FieldEvent] == string(ev) {
			out = append(out, m)
		}
	}
	return out
}

// Heartbeat is the default heartbeat interval of an Env: 10 s scaled, so
// leases live 30 s or more and survive a starved runner. Tests that wait for
// a heartbeat to carry news (a 410, a rejection) wait up to this long.
var Heartbeat = testscale.Seconds(10 * time.Second)

// FastHeartbeat is the heartbeat interval of FastLeases: 1 s scaled.
var FastHeartbeat = testscale.Seconds(time.Second)

// FastLeases sets the shortest heartbeat interval, FastHeartbeat (leases of
// three times that). Use it only where lease expiry is the subject, and keep
// such a test correct if a healthy worker's lease expires too: a loaded
// runner can starve a heartbeat for longer than a short lease lives (#43).
func FastLeases(c *server.Config, _ *store.Config) { c.HeartbeatInterval = FastHeartbeat }

// LeaseTTL is how long a lease lives without a heartbeat in an Env
// configured with c.
func LeaseTTL(c server.Config) time.Duration { return 3 * c.HeartbeatInterval }

// Option adjusts the server and store configuration before the first start.
type Option func(*server.Config, *store.Config)

// Env is a running server plus the helpers to drive it.
type Env struct {
	T   testing.TB
	Dir string // the server's data directory
	// Cfg and StoreCfg are what the server is (re)started with.
	Cfg      server.Config
	StoreCfg store.Config
	// URL is the server's base URL; it stays the same across Restart.
	URL string
	// AdminToken is a client token with admin on *.
	AdminToken string
	// Admin is a client authenticated with AdminToken.
	Admin *client.Client
	// Logs captures the server's JSONL log, across restarts.
	Logs *SyncBuffer
	// Metrics is the server's metrics recorder, kept across restarts.
	Metrics *obs.Metrics
	// Fakes are the fake whisper-cli and ffmpeg.
	Fakes workertest.Fakes

	addr   string
	mu     sync.Mutex
	srv    *server.Server
	st     *store.Store
	cancel context.CancelFunc
	served chan error
}

// Password is the bootstrap admin password.
const Password = "e2e-admin-password"

// New starts a server with fast test timings: Heartbeat (leases long
// enough to survive a starved runner; use FastLeases where expiry is the
// subject), a 100 ms reaper, 2 s claim long-polls, the fake model allowlisted and
// already in the server's model cache, and no VAD model.
func New(t testing.TB, opts ...Option) *Env {
	t.Helper()
	e := &Env{T: t, Dir: t.TempDir(), Logs: &SyncBuffer{}, Metrics: obs.NewMetrics(metrics.NewSet()),
		Fakes: workertest.InstallFakes(t)}
	cfg := server.DefaultConfig()
	cfg.DataDir = e.Dir
	cfg.HeartbeatInterval = Heartbeat
	cfg.PollInterval = time.Second
	cfg.MaxClaimWait = 2 * time.Second
	cfg.TaskTarget = 4 * time.Second
	cfg.SplitWindow = 500 * time.Millisecond
	cfg.Models = []string{Model.Name}
	cfg.VADModel = ""
	cfg.PasswordParams = auth.PasswordParams{MemoryKiB: 64, Time: 1, Threads: 1}
	cfg.ReapInterval = 100 * time.Millisecond
	cfg.StatsInterval = 200 * time.Millisecond
	cfg.ShutdownTimeout = testscale.D(5 * time.Second)
	cfg.RegisterBurst = 100
	cfg.RegisterInterval = time.Millisecond
	e.StoreCfg = store.Config{Path: server.DBPath(e.Dir)}
	e.Cfg = cfg
	preseed := true
	for _, o := range opts {
		o(&e.Cfg, &e.StoreCfg)
	}
	if e.Cfg.ModelUpstream != "" {
		preseed = false
	}
	if preseed {
		dir := filepath.Join(e.Dir, "models")
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, Model.File), ModelBytes, 0o600))
	}
	e.start()
	c, err := e.srv.Ops().CreateClient(context.Background(), server.CLIActor(), "e2e-admin",
		[]api.Grant{{Role: api.RoleAdmin, Scope: "*"}})
	require.NoError(t, err)
	e.AdminToken = c.Token
	e.Admin = e.Client(c.Token)
	t.Cleanup(e.Stop)
	return e
}

// start opens the store and serves on e.addr (a fresh port the first time).
func (e *Env) start() {
	t := e.T
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, e.StoreCfg)
	require.NoError(t, err)
	_, err = server.Bootstrap(ctx, st, Password, e.Cfg.PasswordParams, io.Discard)
	require.NoError(t, err)
	bs, err := blobs.New(server.BlobsPath(e.Dir))
	require.NoError(t, err)
	logger := obs.NewLogger(zerolog.New(e.Logs).With().Timestamp().Logger(), obs.ModeServer)
	srv, err := server.New(e.Cfg, server.Deps{Store: st, Blobs: bs, Logger: logger, Metrics: e.Metrics,
		FreeBytes: func(string) (int64, error) { return 1 << 40, nil }, LookupModel: LookupModel})
	require.NoError(t, err)
	addr := e.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var ln net.Listener
	// The port of a server that just stopped can take a moment to free up.
	require.Eventually(t, func() bool {
		ln, err = net.Listen("tcp", addr)
		return err == nil
	}, Timeout, 20*time.Millisecond, "listen on %s", addr)
	e.addr = ln.Addr().String()
	e.URL = "http://" + e.addr
	sctx, cancel := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(sctx, ln, nil) }()
	e.mu.Lock()
	e.srv, e.st, e.cancel, e.served = srv, st, cancel, served
	e.mu.Unlock()
	e.Eventually(func() bool {
		resp, err := http.Get(e.URL + "/readyz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, "server never became ready")
}

// Stop shuts the server down (gracefully) and closes its store. It is
// idempotent and registered as a cleanup.
func (e *Env) Stop() {
	e.mu.Lock()
	cancel, served, st := e.cancel, e.served, e.st
	e.cancel, e.served, e.st, e.srv = nil, nil, nil, nil
	e.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			e.T.Errorf("server Serve: %v", err)
		}
	case <-time.After(testscale.D(15 * time.Second)):
		e.T.Errorf("server did not stop")
	}
	if err := st.Close(); err != nil {
		e.T.Errorf("closing store: %v", err)
	}
}

// Restart starts the server again on the same address and data directory.
func (e *Env) Restart() { e.start() }

// Server returns the running server.
func (e *Env) Server() *server.Server {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.srv
}

// Client returns an API client for the server with token.
func (e *Env) Client(token string) *client.Client {
	c, err := client.New(e.URL, client.WithToken(token), client.WithHTTPClient(&http.Client{}))
	require.NoError(e.T, err)
	return c
}

// NewClient creates an API client with grants and returns it authenticated.
func (e *Env) NewClient(grants ...api.Grant) *client.Client {
	e.T.Helper()
	c, err := e.Server().Ops().CreateClient(context.Background(), server.CLIActor(), "e2e", grants)
	require.NoError(e.T, err)
	return e.Client(c.Token)
}

// Submitter returns a client with submit on each queue.
func (e *Env) Submitter(queues ...string) *client.Client {
	var gs []api.Grant
	for _, q := range queues {
		gs = append(gs, api.Grant{Role: api.RoleSubmit, Scope: "queue:" + q})
	}
	return e.NewClient(gs...)
}

// CreateQueue creates a queue with weight through the admin API.
func (e *Env) CreateQueue(name string, weight int) {
	e.T.Helper()
	e.AdminJSON(http.MethodPost, "/admin/queues", api.Queue{Name: name, Weight: weight}, http.StatusCreated, nil)
}

// AdminJSON sends a JSON request with the admin token and checks the status.
func (e *Env) AdminJSON(method, path string, in any, want int, out any) {
	e.T.Helper()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		require.NoError(e.T, err)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.URL+path, body)
	require.NoError(e.T, err)
	req.Header.Set("Authorization", "Bearer "+e.AdminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(e.T, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(e.T, want, resp.StatusCode, "%s %s: %s", method, path, b)
	if out != nil {
		require.NoError(e.T, json.Unmarshal(b, out), string(b))
	}
}

// Workers lists the registered workers.
func (e *Env) Workers() []api.WorkerSummary {
	e.T.Helper()
	var ws []api.WorkerSummary
	e.AdminJSON(http.MethodGet, "/admin/workers", nil, http.StatusOK, &ws)
	return ws
}

// WorkerByDescription waits for a worker with this description to register.
func (e *Env) WorkerByDescription(desc string) api.WorkerSummary {
	e.T.Helper()
	var found api.WorkerSummary
	e.Eventually(func() bool {
		for _, w := range e.Workers() {
			if w.Facts.Description == desc {
				found = w
				return true
			}
		}
		return false
	}, "worker "+desc+" never registered")
	return found
}

// Approve makes a worker active and grants it process on queues.
func (e *Env) Approve(id string, queues ...string) {
	e.T.Helper()
	e.AdminJSON(http.MethodPost, "/admin/workers/"+id+"/approve", api.ApproveWorkerRequest{Queues: queues},
		http.StatusNoContent, nil)
}

// SetWorkerState pauses, resumes or rejects a worker (verb is the path tail).
func (e *Env) SetWorkerState(id, verb string) {
	e.T.Helper()
	e.AdminJSON(http.MethodPost, "/admin/workers/"+id+"/"+verb, nil, http.StatusNoContent, nil)
}

// Submit uploads media as a job and returns it.
func (e *Env) Submit(c *client.Client, queue string, p api.JobParams, media []byte) *api.Job {
	e.T.Helper()
	if p.Model == "" {
		p.Model = Model.Name
	}
	j, created, err := c.Submit(context.Background(), queue, p, bytes.NewReader(media), client.SubmitOptions{})
	require.NoError(e.T, err)
	require.True(e.T, created)
	return j
}

// Job fetches a job with the admin client.
func (e *Env) Job(id string) *api.Job {
	e.T.Helper()
	j, err := e.Admin.Job(context.Background(), id)
	require.NoError(e.T, err)
	return j
}

// WaitJob waits until the job reaches a terminal state and returns it.
func (e *Env) WaitJob(id string, want api.JobState) *api.Job {
	e.T.Helper()
	var j *api.Job
	e.Eventually(func() bool {
		j = e.Job(id)
		if want != api.JobFailed && j.State == api.JobFailed {
			e.T.Fatalf("job %s failed: %v", id, deref(j.Error))
		}
		return j.State == want
	}, "job "+id+" never reached "+string(want))
	return j
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// Timeout bounds every wait in these tests: 60 s scaled, longer than two
// default heartbeats.
var Timeout = testscale.D(60 * time.Second)

// Eventually polls cond every 10 ms until it holds, failing after Timeout.
func (e *Env) Eventually(cond func() bool, msg string) {
	e.T.Helper()
	deadline := time.Now().Add(Timeout)
	for !cond() {
		if time.Now().After(deadline) {
			e.T.Fatalf("timed out: %s", msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// WaitEvent waits for a server log event matching match and returns it.
func (e *Env) WaitEvent(ev obs.Event, match func(map[string]any) bool) map[string]any {
	e.T.Helper()
	var got map[string]any
	e.Eventually(func() bool {
		for _, m := range e.Logs.Events(e.T, ev) {
			if match == nil || match(m) {
				got = m
				return true
			}
		}
		return false
	}, "server event "+string(ev)+" never logged")
	return got
}

// Requests returns the server's http.request events for route by principal
// ("" matches any principal).
func (e *Env) Requests(route, principal string) []map[string]any {
	var out []map[string]any
	for _, m := range e.Logs.Events(e.T, obs.EventHTTPRequest) {
		if m[obs.FieldRoute] == route && (principal == "" || m[obs.FieldPrincipal] == principal) {
			out = append(out, m)
		}
	}
	return out
}

// Transcript downloads a finished job's transcript.
func (e *Env) Transcript(c *client.Client, id string) string {
	e.T.Helper()
	rc, err := c.Transcript(context.Background(), id)
	require.NoError(e.T, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(e.T, err)
	return string(b)
}

// Scrape returns the server's metrics exposition.
func (e *Env) Scrape() string {
	var b bytes.Buffer
	e.Metrics.Set().WritePrometheus(&b)
	return b.String()
}

// ErrSimulatedCrash is what a cut worker's transport returns.
var ErrSimulatedCrash = errors.New("simulated crash: network cut")
