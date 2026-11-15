package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"

	"github.com/deweysasser/whisper-network/internal/backoff"
)

// ResumableFile is the destination of a resumable download. *os.File
// implements it.
type ResumableFile interface {
	io.Writer
	io.Seeker
	Truncate(size int64) error
}

// ResumeOptions control DownloadInput's retries.
type ResumeOptions struct {
	// Retry spaces attempts after a transient failure. The attempt count
	// resets whenever an attempt makes progress.
	Retry backoff.Policy
	// MaxAttempts bounds consecutive attempts without progress (default 5).
	MaxAttempts int
	// Sleep waits between attempts (default backoff.Sleep).
	Sleep backoff.Sleeper
	// OnRetry, if set, is told about each retry.
	OnRetry func(attempt int, offset int64, err error)
	// MaxBytes, if positive, bounds the download: a body (or advertised
	// size) beyond it fails with ErrInputTooLarge, which is not retried,
	// after writing at most one byte more.
	MaxBytes int64
}

// ErrInputTooLarge reports an input larger than ResumeOptions.MaxBytes.
var ErrInputTooLarge = errors.New("input larger than the lease allows")

// errShortBody reports a body that ended before the advertised size.
var errShortBody = errors.New("response body shorter than its advertised size")

// DownloadInput copies a lease's input into dst, which must be empty. A
// transient failure mid-stream (network error, 5xx) is retried with
// "Range: bytes=N-" from the bytes already written; when the server answers
// with the whole body instead (transcribe input is regenerated, not ranged),
// dst is truncated and the download restarts. It returns the bytes written.
func (c *Client) DownloadInput(ctx context.Context, leaseID, inputURL string, dst ResumableFile, opt ResumeOptions) (int64, error) {
	u, err := c.inputURL(leaseID, inputURL)
	if err != nil {
		return 0, err
	}
	if opt.MaxAttempts <= 0 {
		opt.MaxAttempts = 5
	}
	if opt.Sleep == nil {
		opt.Sleep = backoff.Sleep
	}
	var written int64
	for attempt := 0; ; {
		progressed, err := c.downloadOnce(ctx, u, dst, &written, opt.MaxBytes)
		if err == nil {
			return written, nil
		}
		if !IsTransient(err) {
			return written, err
		}
		if progressed {
			attempt = 0
		}
		attempt++
		if attempt >= opt.MaxAttempts {
			return written, fmt.Errorf("download input: giving up after %d attempts: %w", attempt, err)
		}
		if opt.OnRetry != nil {
			opt.OnRetry(attempt, written, err)
		}
		if err := opt.Sleep(ctx, opt.Retry.Delay(attempt-1)); err != nil {
			return written, err
		}
	}
}

func (c *Client) downloadOnce(ctx context.Context, u *url.URL, dst ResumableFile, written *int64, maxBytes int64) (progressed bool, err error) {
	d, err := c.open(ctx, u, *written)
	if err != nil {
		return false, err
	}
	defer d.Body.Close()
	if maxBytes > 0 && d.Size > maxBytes {
		return false, fmt.Errorf("download input: %w: server offers %d bytes, limit %d", ErrInputTooLarge, d.Size, maxBytes)
	}
	if d.Offset != *written {
		if d.Offset != 0 {
			return false, fmt.Errorf("download input: server resumed at %d, want %d", d.Offset, *written)
		}
		if _, err := dst.Seek(0, io.SeekStart); err != nil {
			return false, err
		}
		if err := dst.Truncate(0); err != nil {
			return false, err
		}
		*written = 0
	}
	var body io.Reader = d.Body
	if maxBytes > 0 {
		// One byte past the limit tells "exactly the limit" from "more".
		body = io.LimitReader(body, maxBytes-*written+1)
	}
	src := &readErrReader{r: body}
	n, err := io.Copy(dst, src)
	*written += n
	switch {
	case maxBytes > 0 && *written > maxBytes:
		return n > 0, fmt.Errorf("download input: %w: more than %d bytes", ErrInputTooLarge, maxBytes)
	case err != nil && src.err == nil:
		// A write error: the disk, not the network. Not transient.
		return n > 0, fmt.Errorf("download input: writing: %w", err)
	case err != nil:
		return n > 0, err
	case d.Size >= 0 && *written != d.Size:
		return n > 0, errShortBody
	}
	return n > 0, nil
}

// readErrReader remembers the last read error so write errors can be told
// apart from network errors after io.Copy.
type readErrReader struct {
	r   io.Reader
	err error
}

func (r *readErrReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}
