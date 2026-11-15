package ui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

// A worker's page shows its reported disk readings and cached models, and
// nothing for a worker that reported none.
func TestWorkersPageShowsDisk(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	models, work, free := int64(3<<30), int64(512<<20), int64(40<<30)
	require.NoError(t, f.st.TouchWorker(f.ctx, f.w1, &api.Gauges{
		ModelsBytes: &models, WorkBytes: &work, StateFreeBytes: &free,
		CachedModels: []api.CachedModel{{Name: "small.en", Bytes: 487 << 20}, {Name: "silero-v5.1.2", Bytes: 885 << 10}},
	}))
	b := f.login("root")
	resp, body := b.get("/ui/workers/" + f.w1)
	require.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, body, "models 3.0 GiB · work 512.0 MiB · 40.0 GiB free")
	assert.Contains(t, body, "small.en (487.0 MiB), silero-v5.1.2 (885.0 KiB)")
	assert.Equal(t, 1, strings.Count(body, `class="disk"`))
	resp, body = b.get("/ui/workers/" + f.w2)
	require.Equal(t, 200, resp.StatusCode)
	assert.NotContains(t, body, `class="disk"`, "only the worker that reported disk readings")

	// A probe that failed leaves free space unknown.
	require.NoError(t, f.st.TouchWorker(f.ctx, f.w1, &api.Gauges{ModelsBytes: &models, WorkBytes: &work}))
	_, body = b.get("/ui/workers/" + f.w1)
	assert.Contains(t, body, "models 3.0 GiB · work 512.0 MiB · ? free")
}
