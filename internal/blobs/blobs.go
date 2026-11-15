// Package blobs stores job content on disk under <root>/<job-id>/ (DESIGN.md
// §3). The database records sizes and hashes; this package only moves bytes.
//
// Every write is crash safe: content goes to a uniquely named ".part" file
// that is fsynced, renamed over the final name, and the directory fsynced.
// A crash therefore leaves at most an orphan ".part" file, which SweepParts
// removes. Unique part names also let two writers race for the same final
// name without corrupting each other's partial output.
package blobs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Blob names within a job directory.
const (
	// Original is the upload exactly as received.
	Original = "original"
	// Canonical is the 16 kHz mono 16-bit FLAC.
	Canonical = "canonical.flac"
	// CanonicalIndex is the seek index of Canonical (see EncodeIndex).
	CanonicalIndex = "canonical.idx"
	// Transcript is the assembled text transcript with its provenance header.
	Transcript = "transcript.txt"
	// Result is the assembled JSON result with absolute times.
	Result = "result.json"

	partSuffix = ".part"
)

// ErrBadName is returned for a job id or blob name that could escape the
// blob directory.
var ErrBadName = errors.New("blobs: invalid job id or blob name")

var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Store is a blob directory. It is safe for concurrent use; callers that
// need to order operations on one job (such as accepting canonical audio)
// serialize them themselves.
type Store struct {
	root string
}

// New returns a Store rooted at root, creating the directory if needed.
func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("blobs: %w", err)
	}
	return &Store{root: root}, nil
}

// Root returns the blob root directory.
func (s *Store) Root() string { return s.root }

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) &&
		!strings.HasSuffix(name, partSuffix)
}

func (s *Store) dir(jobID string) (string, error) {
	if !idRE.MatchString(jobID) {
		return "", ErrBadName
	}
	return filepath.Join(s.root, jobID), nil
}

// Path returns the file path of a blob.
func (s *Store) Path(jobID, name string) (string, error) {
	d, err := s.dir(jobID)
	if err != nil || !validName(name) {
		return "", ErrBadName
	}
	return filepath.Join(d, name), nil
}

// Pending is a blob being written. Write to it, then Commit or Abort.
type Pending struct {
	*os.File
	final string
	done  bool
}

// Create starts writing a blob. The job directory is created if needed.
func (s *Store) Create(jobID, name string) (*Pending, error) {
	final, err := s.Path(jobID, name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return nil, fmt.Errorf("blobs: %w", err)
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	f, err := os.OpenFile(final+"."+hex.EncodeToString(rnd[:])+partSuffix, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("blobs: %w", err)
	}
	return &Pending{File: f, final: final}, nil
}

// Commit fsyncs the content and atomically publishes it under its final
// name, replacing any previous blob of that name.
func (p *Pending) Commit() error {
	if p.done {
		return errors.New("blobs: pending blob already finished")
	}
	p.done = true
	part := p.Name()
	if err := p.Sync(); err != nil {
		_ = p.Close()
		_ = os.Remove(part)
		return fmt.Errorf("blobs: sync: %w", err)
	}
	if err := p.Close(); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("blobs: close: %w", err)
	}
	if err := os.Rename(part, p.final); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("blobs: publish: %w", err)
	}
	return syncDir(filepath.Dir(p.final))
}

// Abort discards the content. It is safe to call after Commit (a no-op), so
// it can be deferred.
func (p *Pending) Abort() {
	if p.done {
		return
	}
	p.done = true
	_ = p.Close()
	_ = os.Remove(p.Name())
}

// WriteFile atomically writes a small blob.
func (s *Store) WriteFile(jobID, name string, data []byte) error {
	p, err := s.Create(jobID, name)
	if err != nil {
		return err
	}
	defer p.Abort()
	if _, err := p.Write(data); err != nil {
		return fmt.Errorf("blobs: write: %w", err)
	}
	return p.Commit()
}

// ReadFile reads a small blob.
func (s *Store) ReadFile(jobID, name string) ([]byte, error) {
	path, err := s.Path(jobID, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// Open opens a blob for reading. A missing blob matches fs.ErrNotExist.
func (s *Store) Open(jobID, name string) (*os.File, error) {
	path, err := s.Path(jobID, name)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

// Rename atomically renames one blob of a job to another name.
func (s *Store) Rename(jobID, from, to string) error {
	src, err := s.Path(jobID, from)
	if err != nil {
		return err
	}
	dst, err := s.Path(jobID, to)
	if err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("blobs: rename: %w", err)
	}
	return syncDir(filepath.Dir(dst))
}

// Remove deletes the named blobs of a job. Missing blobs are not an error.
func (s *Store) Remove(jobID string, names ...string) error {
	var errs []error
	for _, n := range names {
		path, err := s.Path(jobID, n)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RemoveJob deletes a job's whole directory. A missing directory is not an
// error.
func (s *Store) RemoveJob(jobID string) error {
	d, err := s.dir(jobID)
	if err != nil {
		return err
	}
	return os.RemoveAll(d)
}

// JobDir is one job directory found on disk.
type JobDir struct {
	ID      string
	ModTime time.Time
}

// Jobs lists the job directories present on disk.
func (s *Store) Jobs() ([]JobDir, error) {
	ents, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("blobs: %w", err)
	}
	var out []JobDir
	for _, e := range ents {
		if !e.IsDir() || !idRE.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed concurrently
		}
		out = append(out, JobDir{ID: e.Name(), ModTime: info.ModTime()})
	}
	return out, nil
}

// SweepParts deletes ".part" files last modified before cutoff, the leftovers
// of crashed or abandoned writes, and returns how many it removed. Pass a
// cutoff in the past so writes still in progress are left alone.
func (s *Store) SweepParts(cutoff time.Time) (int, error) {
	n := 0
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), partSuffix) {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			return nil
		}
		if err := os.Remove(path); err == nil {
			n++
		}
		return nil
	})
	return n, err
}

// Usage is the stored size of each blob kind.
type Usage struct {
	Original, Canonical, Transcript int64
}

// Usage walks the blob directory and sums file sizes by kind. Transcript
// counts both the text and the JSON result; the seek index counts as
// canonical.
func (s *Store) Usage() (Usage, error) {
	var u Usage
	err := filepath.WalkDir(s.root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		switch d.Name() {
		case Original:
			u.Original += info.Size()
		case Canonical, CanonicalIndex:
			u.Canonical += info.Size()
		case Transcript, Result:
			u.Transcript += info.Size()
		}
		return nil
	})
	return u, err
}

// syncDir fsyncs a directory so a rename in it is durable. Windows cannot
// open directories for syncing; NTFS journals renames itself.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("blobs: sync dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("blobs: sync dir: %w", err)
	}
	return nil
}
