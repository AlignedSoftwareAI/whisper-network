package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CredentialsFile is the name of the credentials file in the state dir.
const CredentialsFile = "credentials.json"

// Credentials identify this worker to one server. The token is secret: the
// file is written 0600 and the value is never logged.
type Credentials struct {
	Server       string    `json:"server"`
	ID           string    `json:"id"`
	Token        string    `json:"token"`
	RegisteredAt time.Time `json:"registered_at"`
}

// LoadCredentials reads path. A missing file yields an error matching
// os.ErrNotExist. Group or world permissions on the file are removed.
func LoadCredentials(path string) (*Credentials, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.ID == "" || c.Token == "" {
		return nil, fmt.Errorf("%s: missing id or token", path)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(path, 0o600)
	}
	return &c, nil
}

// SaveCredentials writes c to path atomically with mode 0600: a temporary
// file in the same directory is written, synced and renamed over path, and the
// directory is synced, so a crash leaves either the old file or the new one.
func SaveCredentials(path string, c *Credentials) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o600)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir makes a rename durable. Platforms that cannot sync a directory
// (Windows) are not an error.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	_ = d.Sync() // best effort: some filesystems and platforms reject it
	return nil
}
