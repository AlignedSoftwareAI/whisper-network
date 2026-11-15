package worker

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Bounds on what the worker reads back from a child.
const (
	// maxWhisperJSON bounds whisper-cli's JSON. Ten hours of speech is a
	// few MiB.
	maxWhisperJSON = 64 << 20
)

// errBadOutput marks a child output the worker refuses to read.
var errBadOutput = errors.New("refusing child output")

// openOutput opens a file a child wrote in its work directory without
// trusting it: the final path component must not be a symlink (O_NOFOLLOW),
// opening must not block (O_NONBLOCK, against a planted FIFO), and the result
// must be a regular file with a single link and at most max bytes. A child
// that plants a link to /dev/zero, or to a file only the worker can read,
// gets its task failed instead of the worker reading or uploading through it.
// It returns the open file and its size.
func openOutput(path string, max int64) (*os.File, int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|openOutputFlags, 0)
	if err != nil {
		if isSymlinkErr(err) {
			return nil, 0, fmt.Errorf("%w: %s is a symlink", errBadOutput, path)
		}
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	switch {
	case !fi.Mode().IsRegular():
		err = fmt.Errorf("%w: %s is not a regular file (%s)", errBadOutput, path, fi.Mode().Type())
	case linkCount(fi) > 1:
		err = fmt.Errorf("%w: %s has %d links", errBadOutput, path, linkCount(fi))
	case fi.Size() > max:
		err = fmt.Errorf("%w: %s is %d bytes, limit %d", errBadOutput, path, fi.Size(), max)
	}
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// readOutput reads a child output checked by openOutput, never more than max
// bytes even if the file grows meanwhile.
func readOutput(path string, max int64) ([]byte, error) {
	f, _, err := openOutput(path, max)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%w: %s grew beyond %d bytes", errBadOutput, path, max)
	}
	return b, nil
}
