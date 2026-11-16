package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDiskCeiling(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]DiskCeiling{
		"80%": {Percent: 80}, " 12.5 % ": {Percent: 12.5}, "100%": {Percent: 100}, "200GiB": {Bytes: 200 << 30},
	} {
		got, err := ParseDiskCeiling(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"0%", "101%", "-5%", "x%", "", "0", "lots"} {
		_, err := ParseDiskCeiling(bad)
		assert.Error(t, err, bad)
	}
	assert.Equal(t, int64(800), DiskCeiling{Percent: 80}.Limit(1000))
	assert.Equal(t, int64(5), DiskCeiling{Bytes: 5}.Limit(1000))
}
