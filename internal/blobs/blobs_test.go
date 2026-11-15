package blobs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/media"
)

func pt(sample, offset int64) media.SeekPoint {
	return media.SeekPoint{Sample: sample, Offset: offset}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	return s
}

func TestCommitPublishesAtomically(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	p, err := s.Create("job1", Original)
	require.NoError(t, err)
	_, err = p.WriteString("hello")
	require.NoError(t, err)

	_, err = s.Open("job1", Original)
	assert.ErrorIs(t, err, os.ErrNotExist, "nothing visible before commit")
	assert.True(t, strings.HasSuffix(p.Name(), ".part"))

	require.NoError(t, p.Commit())
	p.Abort() // no-op after commit
	b, err := s.ReadFile("job1", Original)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(b))
	assert.Error(t, p.Commit(), "commit twice")

	ents, err := os.ReadDir(filepath.Join(s.Root(), "job1"))
	require.NoError(t, err)
	assert.Len(t, ents, 1, "no part file left behind")
}

func TestAbortRemovesPart(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	p, err := s.Create("job1", Canonical)
	require.NoError(t, err)
	p.Abort()
	_, err = os.Stat(p.Name())
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestConcurrentWritersDoNotCollide(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a, err := s.Create("job1", Canonical)
	require.NoError(t, err)
	b, err := s.Create("job1", Canonical)
	require.NoError(t, err)
	assert.NotEqual(t, a.Name(), b.Name())
	_, _ = a.WriteString("a")
	_, _ = b.WriteString("b")
	require.NoError(t, a.Commit())
	require.NoError(t, b.Commit())
	got, err := s.ReadFile("job1", Canonical)
	require.NoError(t, err)
	assert.Equal(t, "b", string(got))
}

func TestRejectsEscapingNames(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	for _, c := range []struct{ job, name string }{
		{"../x", Original}, {"a/b", Original}, {"", Original}, {".", Original},
		{"job", "../../etc/passwd"}, {"job", ""}, {"job", "x.part"}, {"job", `a\b`},
	} {
		_, err := s.Path(c.job, c.name)
		assert.ErrorIs(t, err, ErrBadName, "%q %q", c.job, c.name)
	}
	assert.ErrorIs(t, s.RemoveJob(".."), ErrBadName)
}

func TestRenameRemoveAndJobs(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	require.NoError(t, s.WriteFile("j1", Original, []byte("abc")))
	require.NoError(t, s.WriteFile("j2", Transcript, []byte("t")))
	require.NoError(t, s.Rename("j1", Original, Canonical))
	_, err := s.Open("j1", Original)
	assert.ErrorIs(t, err, os.ErrNotExist)

	u, err := s.Usage()
	require.NoError(t, err)
	assert.Equal(t, Usage{Canonical: 3, Transcript: 1}, u)

	jobs, err := s.Jobs()
	require.NoError(t, err)
	var ids []string
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	assert.ElementsMatch(t, []string{"j1", "j2"}, ids)

	require.NoError(t, s.Remove("j1", Canonical, Original)) // missing Original is fine
	require.NoError(t, s.RemoveJob("j2"))
	require.NoError(t, s.RemoveJob("j2"), "removing twice is fine")
	jobs, err = s.Jobs()
	require.NoError(t, err)
	assert.Len(t, jobs, 1)
}

func TestSweepParts(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	old, err := s.Create("j1", Original)
	require.NoError(t, err)
	require.NoError(t, old.Close())
	past := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(old.Name(), past, past))
	fresh, err := s.Create("j1", Canonical)
	require.NoError(t, err)
	defer fresh.Abort()
	require.NoError(t, s.WriteFile("j1", Transcript, []byte("keep")))

	n, err := s.SweepParts(time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, err = os.Stat(old.Name())
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(fresh.Name())
	assert.NoError(t, err, "in-progress write survives")
	_, err = s.ReadFile("j1", Transcript)
	assert.NoError(t, err)
}

func TestIndexRoundTrip(t *testing.T) {
	t.Parallel()
	x := &media.Index{Samples: 100000, Points: []media.SeekPoint{pt(0, 42), pt(16000, 9000), pt(32768, 20000)}}
	got, err := DecodeIndex(EncodeIndex(x))
	require.NoError(t, err)
	assert.Equal(t, x, got)

	empty := &media.Index{Samples: 10, Points: []media.SeekPoint{}}
	got, err = DecodeIndex(EncodeIndex(empty))
	require.NoError(t, err)
	assert.Equal(t, empty, got)
}

func TestIndexRejectsCorruption(t *testing.T) {
	t.Parallel()
	good := EncodeIndex(&media.Index{Samples: 100000, Points: []media.SeekPoint{pt(0, 42), pt(16000, 9000)}})
	for name, b := range map[string][]byte{
		"empty":     nil,
		"magic":     append([]byte("XXXX"), good[4:]...),
		"truncated": good[:len(good)-1],
		"bitflip": func() []byte {
			c := append([]byte(nil), good...)
			c[6] ^= 1
			return c
		}(),
	} {
		_, err := DecodeIndex(b)
		assert.ErrorIs(t, err, ErrBadIndex, name)
	}
	// Points beyond the sample total are refused even with a valid checksum.
	_, err := DecodeIndex(EncodeIndex(&media.Index{Samples: 10, Points: []media.SeekPoint{pt(0, 1), pt(20, 5)}}))
	assert.ErrorIs(t, err, ErrBadIndex)
	_, err = DecodeIndex(EncodeIndex(&media.Index{Samples: 100, Points: []media.SeekPoint{pt(0, 1), pt(0, 5)}}))
	assert.ErrorIs(t, err, ErrBadIndex, "repeated sample")
}

// TestBlobsAreOwnerOnly: media and transcripts are readable by the server's
// user alone, whatever the process umask, in directories only it can list.
func TestBlobsAreOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	t.Parallel()
	root := filepath.Join(t.TempDir(), "blobs")
	s, err := New(root)
	require.NoError(t, err)
	require.NoError(t, s.WriteFile("job1", Transcript, []byte("secret words")))
	for path, want := range map[string]os.FileMode{
		root:                                    0o700,
		filepath.Join(root, "job1"):             0o700,
		filepath.Join(root, "job1", Transcript): 0o600,
	} {
		fi, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, want, fi.Mode().Perm(), path)
	}
}
