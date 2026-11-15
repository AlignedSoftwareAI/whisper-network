package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/models"
)

// modelFetchTimeout bounds one upstream download (the largest pinned model
// is about 3 GB).
const modelFetchTimeout = 2 * time.Hour

// modelCache serves pinned model files from DataDir/models, filling it from
// upstream on first request. A file enters the cache only after its size and
// sha256 match the pinned values, via an atomic rename, so the cache never
// holds a partial or tampered file. Concurrent requests for a missing model
// share one download.
type modelCache struct {
	dir      string
	upstream string
	client   *http.Client
	log      zerolog.Logger

	mu       sync.Mutex
	inflight map[string]*modelFetch
	fetches  sync.WaitGroup // detached downloads, joined by wait
}

type modelFetch struct {
	done chan struct{}
	err  error
}

func newModelCache(dir, upstream string, client *http.Client, log zerolog.Logger) *modelCache {
	return &modelCache{dir: dir, upstream: upstream, client: client, log: log, inflight: map[string]*modelFetch{}}
}

func (c *modelCache) path(m models.Model) string { return filepath.Join(c.dir, filepath.Base(m.File)) }

func (c *modelCache) url(m models.Model) string {
	if c.upstream != "" {
		return c.upstream + "/" + m.File
	}
	return m.URL
}

// ensure returns the cached path of m, downloading it if needed. The
// download runs under bg (the server's lifetime), not the request, so one
// impatient caller cannot abort the fetch others are waiting for.
func (c *modelCache) ensure(ctx, bg context.Context, m models.Model) (string, error) {
	p := c.path(m)
	if fi, err := os.Stat(p); err == nil && fi.Size() == m.Size {
		return p, nil
	}
	c.mu.Lock()
	f := c.inflight[m.Name]
	if f == nil {
		if err := bg.Err(); err != nil { // shutting down: start nothing new
			c.mu.Unlock()
			return "", err
		}
		f = &modelFetch{done: make(chan struct{})}
		c.inflight[m.Name] = f
		c.fetches.Go(func() {
			// Waiters must be released however the download ends, a
			// panic included.
			defer func() {
				if v := recover(); v != nil {
					f.err = fmt.Errorf("fetch model %s: panic: %v", m.Name, v)
				}
				c.mu.Lock()
				delete(c.inflight, m.Name)
				c.mu.Unlock()
				close(f.done)
			}()
			f.err = c.download(bg, m)
		})
	}
	c.mu.Unlock()
	select {
	case <-f.done:
		return p, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// wait blocks until detached downloads have finished; cancel their context
// first (Server.Close does).
func (c *modelCache) wait() { c.fetches.Wait() }

func (c *modelCache) download(ctx context.Context, m models.Model) error {
	ctx, cancel := context.WithTimeout(ctx, modelFetchTimeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(m), nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch model %s: %w", m.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch model %s: upstream status %s", m.Name, resp.Status)
	}
	if err := os.MkdirAll(c.dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dir, filepath.Base(m.File)+".*.part")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after the rename
	defer tmp.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, m.Size+1))
	if err != nil {
		return fmt.Errorf("fetch model %s: %w", m.Name, err)
	}
	if n != m.Size {
		return fmt.Errorf("fetch model %s: got %d bytes, pinned size is %d", m.Name, n, m.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != m.SHA256 {
		return fmt.Errorf("fetch model %s: sha256 %s does not match the pinned %s", m.Name, got, m.SHA256)
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), c.path(m)); err != nil {
		return err
	}
	c.log.Info().Str("model_name", m.Name).Int64("bytes", n).Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("model cached")
	return nil
}

// handleModel is GET /models/{name}: an allowlisted model file (or the VAD
// model) for active workers, with its sha256 as the ETag.
func (s *Server) handleModel(w http.ResponseWriter, r *http.Request, p *Principal) error {
	// A paused worker still finishes its running leases (DESIGN.md §4.2),
	// and a transcribe lease claimed just before the pause may not have
	// fetched its model yet; only pending (and rejected) workers are refused.
	if st := p.Worker.State; st != api.WorkerPaused {
		if prob := claimState(st); prob != nil {
			return prob
		}
	}
	if !auth.Can(p.Principal, auth.ActFetchModel, auth.Resource{}) {
		return s.deny(r, p, "model fetch not allowed")
	}
	name := r.PathValue("name")
	m, ok := s.allowed[name]
	if !ok && s.vad != nil && s.vad.Name == name {
		m, ok = *s.vad, true
	}
	if !ok {
		return newProblem(http.StatusNotFound, api.ProblemNotFound, "model "+name+" is not served here")
	}
	path, err := s.models.ensure(r.Context(), s.bg, m)
	if err != nil {
		if r.Context().Err() != nil {
			return nil // the worker went away
		}
		s.log.Warn().Err(err).Str("model_name", name).Msg("model unavailable")
		return newProblem(http.StatusBadGateway, api.ProblemInternal, "model "+name+" is unavailable from upstream")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", strconv.Quote(m.SHA256))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	s.extendWrite(w, fi.Size())
	http.ServeContent(w, r, "", fi.ModTime(), f)
	return nil
}
