package e2e

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/e2e/e2etest"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
)

// upstream is a fake model host (Hugging Face) that counts fetches.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newUpstream(t *testing.T, files map[string][]byte) *upstream {
	u := &upstream{hits: map[string]int{}}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		u.mu.Lock()
		u.hits[name]++
		u.mu.Unlock()
		b, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) count(file string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[file]
}

// TestModelDownloadPath (scenario 8): the server fetches a pinned model (and
// the VAD model) from --model-upstream on first request, verifies and
// caches it; the worker downloads it from the server, verifies and caches
// it too; a second job reuses both caches.
func TestModelDownloadPath(t *testing.T) {
	t.Parallel()
	up := newUpstream(t, map[string][]byte{e2etest.Model.File: e2etest.ModelBytes, e2etest.VAD.File: e2etest.VADBytes})
	e := e2etest.New(t, func(c *server.Config, _ *store.Config) {
		c.ModelUpstream = up.URL + "/mirror"
		c.VADModel = e2etest.VAD.Name
	})
	_, err := os.Stat(filepath.Join(e.Dir, "models", e2etest.Model.File))
	require.ErrorIs(t, err, os.ErrNotExist, "the server starts with an empty model cache")

	w := e.StartWorker("downloader")
	e.Approve(w.ID(), "public")
	sub := e.Submitter("public")
	first := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	e.WaitJob(first.ID, api.JobDone)
	assert.Contains(t, e.Transcript(sub, first.ID), "; vad="+e2etest.VAD.Name+"; ")

	for _, m := range []string{e2etest.Model.File, e2etest.VAD.File} {
		b, err := os.ReadFile(filepath.Join(e.Dir, "models", m))
		require.NoError(t, err, "server cache")
		assert.NotEmpty(t, b)
		b, err = os.ReadFile(filepath.Join(w.Cfg.StateDir, "models", m))
		require.NoError(t, err, "worker cache")
		assert.NotEmpty(t, b)
	}

	second := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	e.WaitJob(second.ID, api.JobDone)
	assert.Equal(t, 1, up.count(e2etest.Model.File), "the server fetched the model once")
	assert.Equal(t, 1, up.count(e2etest.VAD.File), "the server fetched the VAD model once")
	assert.Len(t, e.Requests("/models/{name}", ""), 2, "the worker fetched each model once")
	for _, r := range e.Requests("/models/{name}", "") {
		assert.Equal(t, float64(200), r["status"])
	}
}
