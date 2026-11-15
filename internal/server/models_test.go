package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/store"
)

// fakeUpstream serves fakeModelBytes under any file name, counting
// requests and holding each one until release is closed.
type fakeUpstream struct {
	*httptest.Server
	hits    atomic.Int64
	release chan struct{}
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	u := &fakeUpstream{release: make(chan struct{})}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		<-u.release
		_, _ = w.Write(fakeModelBytes)
	}))
	t.Cleanup(u.Close)
	return u
}

func TestModelsEndpoint(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t)
	h := newHarness(t, func(c *Config, _ *store.Config) { c.ModelUpstream = up.URL })
	_, pendingTok := h.worker(api.WorkerFacts{})
	_, wtok := h.activeWorker(api.WorkerFacts{}, "public")

	resp, b := h.call("GET", "/models/"+fakeModel.Name, pendingTok, nil)
	h.expectProblem(resp, b, http.StatusForbidden, api.ProblemWorkerPending)
	resp, b = h.call("GET", "/models/large-v3", wtok, nil)
	h.expectProblem(resp, b, http.StatusNotFound, api.ProblemNotFound)

	// Concurrent first requests share one upstream download.
	const n = 5
	var wg sync.WaitGroup
	bodies := make([][]byte, n)
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := http.NewRequest("GET", h.ts.URL+"/models/"+fakeModel.Name, nil)
			r.Header.Set("Authorization", "Bearer "+wtok)
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			codes[i] = resp.StatusCode
			bodies[i], _ = io.ReadAll(resp.Body)
		}()
	}
	require.Eventually(t, func() bool { return up.hits.Load() == 1 }, 2*time.Second, 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond) // the others are now waiting on the same fetch
	close(up.release)
	wg.Wait()
	for i := range n {
		assert.Equal(t, http.StatusOK, codes[i])
		assert.Equal(t, fakeModelBytes, bodies[i])
	}
	assert.Equal(t, int64(1), up.hits.Load(), "singleflight")

	// Cached: served with the pinned hash as ETag, no upstream traffic.
	resp = h.do(req{method: "GET", path: "/models/" + fakeModel.Name, token: wtok})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, strconv.Quote(fakeModel.SHA256), resp.Header.Get("ETag"))
	resp = h.do(req{method: "GET", path: "/models/" + fakeModel.Name, token: wtok,
		header: map[string]string{"If-None-Match": strconv.Quote(fakeModel.SHA256)}})
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	resp = h.do(req{method: "GET", path: "/models/" + fakeModel.Name, token: wtok,
		header: map[string]string{"Range": "bytes=0-9"}})
	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Equal(t, int64(1), up.hits.Load())

	// A hash mismatch is never cached.
	resp, b = h.call("GET", "/models/"+badModel.Name, wtok, nil)
	h.expectProblem(resp, b, http.StatusBadGateway, api.ProblemInternal)
	_, err := os.Stat(filepath.Join(h.dir, "models", badModel.File))
	assert.ErrorIs(t, err, os.ErrNotExist)
	ents, err := os.ReadDir(filepath.Join(h.dir, "models"))
	require.NoError(t, err)
	assert.Len(t, ents, 1, "no partial download left behind")
}

func TestModelUpstreamFailure(t *testing.T) {
	t.Parallel()
	up := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(up.Close)
	h := newHarness(t, func(c *Config, _ *store.Config) { c.ModelUpstream = up.URL })
	_, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	resp, b := h.call("GET", "/models/"+fakeModel.Name, wtok, nil)
	h.expectProblem(resp, b, http.StatusBadGateway, api.ProblemInternal)
}

// TestModelsServedToPausedWorker: a paused worker finishes its running
// leases, which may still need their model (found by the e2e suite: the
// worker failed the lease on a 409 here).
func TestModelsServedToPausedWorker(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t)
	close(up.release)
	h := newHarness(t, func(c *Config, _ *store.Config) { c.ModelUpstream = up.URL })
	id, tok := h.activeWorker(api.WorkerFacts{}, "public")
	h.expect("POST", "/admin/workers/"+id+"/pause", h.admin, nil, http.StatusNoContent, nil)
	resp, b := h.call("GET", "/models/"+fakeModel.Name, tok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	assert.Equal(t, fakeModelBytes, b)
}
