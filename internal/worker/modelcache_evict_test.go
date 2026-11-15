package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/models"
)

// sizedModel returns a whisper model of n bytes and its data.
func sizedModel(name string, n int) (models.Model, []byte) {
	data := bytes.Repeat([]byte{byte(len(name))}, n)
	sum := sha256.Sum256(data)
	return models.Model{Name: name, Kind: models.KindWhisper, File: "ggml-" + name + ".bin",
		SHA256: hex.EncodeToString(sum[:]), Size: int64(n)}, data
}

type modelServer map[string][]byte

func (s modelServer) fetch(_ context.Context, m models.Model) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s[m.Name])), nil
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// setUsed backdates a model file's last use.
func setUsed(t *testing.T, path string, at time.Time) {
	t.Helper()
	require.NoError(t, os.Chtimes(path, at, at))
}

func TestModelCacheEvictsLeastRecentlyUsed(t *testing.T) {
	a, da := sizedModel("a", 400)
	b, db := sizedModel("b", 400)
	c, dc := sizedModel("c", 400)
	srv := modelServer{"a": da, "b": db, "c": dc}
	dir := t.TempDir()
	cache := NewModelCache(t.Context(), dir, srv.fetch).WithMax(1000)
	ctx := t.Context()

	pa, err := cache.Path(ctx, a)
	require.NoError(t, err)
	pb, err := cache.Path(ctx, b)
	require.NoError(t, err)
	old := time.Now().Add(-time.Hour)
	setUsed(t, pa, old.Add(time.Minute)) // a used after b
	setUsed(t, pb, old)

	pc, err := cache.Path(ctx, c)
	require.NoError(t, err)
	assert.True(t, exists(t, pa))
	assert.False(t, exists(t, pb), "b was least recently used")
	assert.True(t, exists(t, pc))

	// An evicted model is downloaded again when needed, evicting the next
	// least recently used (a).
	setUsed(t, pa, old)
	_, err = cache.Path(ctx, b)
	require.NoError(t, err)
	assert.True(t, exists(t, pb))
	assert.False(t, exists(t, pa))
}

func TestModelCacheNeverEvictsModelsInUse(t *testing.T) {
	a, da := sizedModel("a", 600)
	b, db := sizedModel("b", 600)
	srv := modelServer{"a": da, "b": db}
	dir := t.TempDir()
	cache := NewModelCache(t.Context(), dir, srv.fetch).WithMax(1000)
	ctx := t.Context()

	pa, releaseA, err := cache.Acquire(ctx, a)
	require.NoError(t, err)
	setUsed(t, pa, time.Now().Add(-time.Hour))
	pb, releaseB, err := cache.Acquire(ctx, b)
	require.NoError(t, err)
	assert.True(t, exists(t, pa), "a is in use: the cap is exceeded rather than failing a task")
	assert.True(t, exists(t, pb))

	releaseB()
	assert.True(t, exists(t, pa))
	assert.False(t, exists(t, pb), "b is the only model not in use")

	releaseA()
	releaseA() // idempotent
	assert.True(t, exists(t, pa), "within the cap once b is gone")
}

func TestModelCacheUnlimitedByDefault(t *testing.T) {
	a, da := sizedModel("a", 600)
	b, db := sizedModel("b", 600)
	dir := t.TempDir()
	cache := NewModelCache(t.Context(), dir, modelServer{"a": da, "b": db}.fetch)
	pa, err := cache.Path(t.Context(), a)
	require.NoError(t, err)
	pb, err := cache.Path(t.Context(), b)
	require.NoError(t, err)
	assert.True(t, exists(t, pa))
	assert.True(t, exists(t, pb))
}

// Files from a previous run are ordered by their modification time, and
// partial downloads are never evicted.
func TestModelCacheEvictsAtStartupKeepingPartials(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	for i, name := range []string{"ggml-old.bin", "ggml-new.bin"} {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, make([]byte, 500), 0o644))
		setUsed(t, p, old.Add(time.Duration(i)*time.Minute))
	}
	partial := filepath.Join(dir, ".ggml-x.bin.part-1")
	require.NoError(t, os.WriteFile(partial, make([]byte, 500), 0o644))
	setUsed(t, partial, old.Add(-time.Hour))

	NewModelCache(t.Context(), dir, nil).WithMax(1000).evict()
	assert.False(t, exists(t, filepath.Join(dir, "ggml-old.bin")))
	assert.True(t, exists(t, filepath.Join(dir, "ggml-new.bin")))
	assert.True(t, exists(t, partial))
}
