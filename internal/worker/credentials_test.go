package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", CredentialsFile)
	_, err := LoadCredentials(path)
	assert.ErrorIs(t, err, os.ErrNotExist)

	c := &Credentials{Server: "https://wn.example", ID: "w1", Token: "wnw_x", RegisteredAt: time.Unix(1, 0).UTC()}
	require.NoError(t, SaveCredentials(path, c))
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	got, err := LoadCredentials(path)
	require.NoError(t, err)
	assert.Equal(t, c, got)

	entries, _ := os.ReadDir(filepath.Dir(path))
	assert.Len(t, entries, 1, "no temporary files left behind")
}

func TestLoadCredentialsTightensPermissionsAndValidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, CredentialsFile)
	require.NoError(t, os.WriteFile(path, []byte(`{"id":"w1","token":"wnw_x"}`), 0o644))
	_, err := LoadCredentials(path)
	require.NoError(t, err)
	fi, _ := os.Stat(path)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	require.NoError(t, os.WriteFile(path, []byte(`{"id":"w1"}`), 0o600))
	_, err = LoadCredentials(path)
	assert.ErrorContains(t, err, "missing id or token")
	require.NoError(t, os.WriteFile(path, []byte(`{`), 0o600))
	_, err = LoadCredentials(path)
	assert.Error(t, err)
}
