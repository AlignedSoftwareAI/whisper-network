package worker

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/models"
)

// countingFetcher serves data for every model, counting fetches; gate, if
// non-nil, holds each fetch until closed.
type countingFetcher struct {
	data []byte
	n    atomic.Int32
	gate chan struct{}
}

func (f *countingFetcher) fetch(_ context.Context, _ models.Model) (io.ReadCloser, error) {
	f.n.Add(1)
	if f.gate != nil {
		<-f.gate
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func TestModelCacheSingleflight(t *testing.T) {
	m := testCatalog()[0]
	f := &countingFetcher{data: tinyData, gate: make(chan struct{})}
	c := NewModelCache(context.Background(), t.TempDir(), f.fetch)

	var wg sync.WaitGroup
	paths := make([]string, 8)
	for i := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := c.Path(context.Background(), m)
			assert.NoError(t, err)
			paths[i] = p
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(f.gate)
	wg.Wait()
	assert.Equal(t, int32(1), f.n.Load(), "concurrent requests share one download")
	for _, p := range paths {
		assert.Equal(t, paths[0], p)
	}
	b, err := os.ReadFile(paths[0])
	require.NoError(t, err)
	assert.Equal(t, tinyData, b)
}

func TestModelCacheRejectsMismatch(t *testing.T) {
	m := testCatalog()[0]
	dir := t.TempDir()
	for name, data := range map[string][]byte{
		"wrong bytes": bytes.Repeat([]byte("x"), len(tinyData)),
		"too long":    append(bytes.Clone(tinyData), 'x'),
		"too short":   tinyData[:10],
	} {
		t.Run(name, func(t *testing.T) {
			c := NewModelCache(context.Background(), dir, (&countingFetcher{data: data}).fetch)
			_, err := c.Path(context.Background(), m)
			assert.ErrorIs(t, err, ErrModelMismatch)
			entries, _ := os.ReadDir(dir)
			assert.Empty(t, entries, "nothing is left under the final or temporary name")
		})
	}
}

func TestModelCacheReusesAndRepairsFilesOnDisk(t *testing.T) {
	m := testCatalog()[0]
	dir := t.TempDir()
	f := &countingFetcher{data: tinyData}
	_, err := NewModelCache(context.Background(), dir, f.fetch).Path(context.Background(), m)
	require.NoError(t, err)

	// A restarted worker verifies the existing file instead of downloading.
	c := NewModelCache(context.Background(), dir, f.fetch)
	p, err := c.Path(context.Background(), m)
	require.NoError(t, err)
	assert.Equal(t, int32(1), f.n.Load())

	// The verdict is cached for the process: no re-hash on later calls.
	require.NoError(t, os.WriteFile(p, []byte("corrupted later"), 0o644))
	_, err = c.Path(context.Background(), m)
	require.NoError(t, err)
	assert.Equal(t, int32(1), f.n.Load())

	// A new process notices the corruption and replaces the file.
	_, err = NewModelCache(context.Background(), dir, f.fetch).Path(context.Background(), m)
	require.NoError(t, err)
	assert.Equal(t, int32(2), f.n.Load())
	b, _ := os.ReadFile(filepath.Join(dir, m.File))
	assert.Equal(t, tinyData, b)
}

func TestModelCacheCallerCanGiveUp(t *testing.T) {
	m := testCatalog()[0]
	f := &countingFetcher{data: tinyData, gate: make(chan struct{})}
	c := NewModelCache(context.Background(), t.TempDir(), f.fetch)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := c.Path(ctx, m)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	close(f.gate)
	// The download carried on and the next caller gets the file.
	_, err = c.Path(context.Background(), m)
	require.NoError(t, err)
	assert.Equal(t, int32(1), f.n.Load())
}

func TestModelCacheRejectsBadCatalogEntry(t *testing.T) {
	c := NewModelCache(context.Background(), t.TempDir(), (&countingFetcher{}).fetch)
	_, err := c.Path(context.Background(), models.Model{Name: "x", File: "../escape", SHA256: "ab"})
	assert.Error(t, err)
}
