package diskfree

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFree(t *testing.T) {
	n, err := Free(t.TempDir())
	require.NoError(t, err)
	assert.Positive(t, n)

	_, err = Free(filepath.Join(t.TempDir(), "missing"))
	assert.Error(t, err)
}
