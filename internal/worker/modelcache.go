package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/deweysasser/whisper-network/internal/models"
)

// ErrModelMismatch reports model bytes that do not match the pinned size or
// sha256.
var ErrModelMismatch = errors.New("model file does not match its pinned hash")

// Fetcher opens a model's bytes from wherever they come from: the server's
// /models/{name} for workers, the upstream URL for bench.
type Fetcher func(ctx context.Context, m models.Model) (io.ReadCloser, error)

// ModelCache keeps verified model files in a directory. Concurrent requests
// for one model share a single download (singleflight). A file already on
// disk is hashed once per process and the verdict cached; a file that fails
// verification is replaced. Downloads go to a temporary name and are renamed
// into place only after size and sha256 match the pinned values, so a crash
// never leaves a corrupt file under the final name.
type ModelCache struct {
	dir   string
	fetch Fetcher
	// base bounds downloads: a download runs to completion even if the
	// caller that started it gives up, so the next lease benefits.
	base context.Context

	mu       sync.Mutex
	verified map[string]bool
	flights  map[string]*flight
}

type flight struct {
	done chan struct{}
	path string
	err  error
}

// NewModelCache returns a cache in dir. Downloads run under base and stop
// when it is cancelled.
func NewModelCache(base context.Context, dir string, fetch Fetcher) *ModelCache {
	return &ModelCache{dir: dir, fetch: fetch, base: base, verified: map[string]bool{}, flights: map[string]*flight{}}
}

// Path returns the local path of a verified copy of m, downloading it if
// needed. ctx bounds only this caller's wait.
func (c *ModelCache) Path(ctx context.Context, m models.Model) (string, error) {
	if m.File == "" || m.SHA256 == "" || filepath.Base(m.File) != m.File {
		return "", fmt.Errorf("model %q: bad catalog entry", m.Name)
	}
	c.mu.Lock()
	if c.verified[m.Name] {
		c.mu.Unlock()
		return c.final(m), nil
	}
	f, ok := c.flights[m.Name]
	if !ok {
		f = &flight{done: make(chan struct{})}
		c.flights[m.Name] = f
		go c.run(m, f)
	}
	c.mu.Unlock()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-f.done:
		return f.path, f.err
	}
}

func (c *ModelCache) final(m models.Model) string { return filepath.Join(c.dir, m.File) }

func (c *ModelCache) run(m models.Model, f *flight) {
	f.path, f.err = c.ensure(m)
	c.mu.Lock()
	if f.err == nil {
		c.verified[m.Name] = true
	}
	delete(c.flights, m.Name)
	c.mu.Unlock()
	close(f.done)
}

func (c *ModelCache) ensure(m models.Model) (string, error) {
	path := c.final(m)
	if err := verifyFile(path, m); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// Corrupt or stale: replace it.
		_ = os.Remove(path)
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return "", err
	}
	if err := c.download(m, path); err != nil {
		return "", fmt.Errorf("model %s: %w", m.Name, err)
	}
	return path, nil
}

func (c *ModelCache) download(m models.Model, path string) (err error) {
	body, err := c.fetch(c.base, m)
	if err != nil {
		return err
	}
	defer body.Close()
	tmp, err := os.CreateTemp(c.dir, "."+m.File+".part-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	// Read one byte past the pinned size so an oversized body is caught
	// without trusting Content-Length.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, m.Size+1))
	if err != nil {
		tmp.Close()
		return err
	}
	if err := checkDigest(m, n, h.Sum(nil)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func verifyFile(path string, m models.Model) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() != m.Size {
		return fmt.Errorf("%w: size %d, want %d", ErrModelMismatch, fi.Size(), m.Size)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	return checkDigest(m, n, h.Sum(nil))
}

func checkDigest(m models.Model, n int64, sum []byte) error {
	if n != m.Size {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrModelMismatch, n, m.Size)
	}
	if got := hex.EncodeToString(sum); got != m.SHA256 {
		return fmt.Errorf("%w: sha256 %s, want %s", ErrModelMismatch, got, m.SHA256)
	}
	return nil
}
