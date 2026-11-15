package media

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// decoder walks the frames of a FLAC stream, tracking sample positions and
// byte offsets itself. It deliberately avoids flac.Stream.Seek, which decodes
// the whole file to build a seek table on first use, and frame.SampleNumber,
// which is wrong for the short final frame of a fixed-block-size stream.
type decoder struct {
	cr   *countReader
	br   *bufio.Reader
	base int64 // byte offset of the reader's first byte within the file
	pos  int64 // sample number of the next frame
	max  int64 // sample limit; decoding past it fails with ErrTooLong
	info *meta.StreamInfo
}

// decoded is one frame's worth of samples.
type decoded struct {
	offset  int64   // byte offset of the frame within the file
	start   int64   // sample number of the first sample
	samples []int32 // valid until the next call to next
}

// newDecoder prepares to read frames from r, which is positioned at file
// offset base. When base is zero it also parses the FLAC metadata.
func newDecoder(r io.Reader, base, max int64) (*decoder, error) {
	cr := &countReader{r: r}
	d := &decoder{cr: cr, br: bufio.NewReaderSize(cr, 64<<10), base: base, max: max}
	if base != 0 {
		return d, nil
	}
	// flac.New wraps its argument in bufio.NewReader, which returns a
	// *bufio.Reader of sufficient size as is, so d.br stays in step with it.
	st, err := flac.New(d.br)
	if err != nil {
		return nil, d.wrap("flac header", err)
	}
	d.info = st.Info
	return d, nil
}

// offset is the file offset of the next unread byte.
func (d *decoder) offset() int64 {
	return d.base + d.cr.n - int64(d.br.Buffered())
}

// canonical reports whether STREAMINFO describes canonical audio.
func (d *decoder) canonical() error {
	si := d.info
	if si.SampleRate != SampleRate || si.NChannels != Channels || si.BitsPerSample != BitsPerSample {
		return fmt.Errorf("%w: FLAC stream is %d Hz, %d ch, %d-bit",
			ErrNotCanonical, si.SampleRate, si.NChannels, si.BitsPerSample)
	}
	return nil
}

// next decodes the following frame, returning io.EOF after the last one.
func (d *decoder) next() (decoded, error) {
	off := d.offset()
	f, err := frame.Parse(d.br)
	if err != nil {
		if err == io.EOF && d.cr.err == nil {
			return decoded{}, io.EOF
		}
		return decoded{}, d.wrap("frame", err)
	}
	if len(f.Subframes) != Channels {
		return decoded{}, corruptf("frame has %d channels", len(f.Subframes))
	}
	if (f.BitsPerSample != 0 && f.BitsPerSample != BitsPerSample) ||
		(f.SampleRate != 0 && f.SampleRate != SampleRate) {
		return decoded{}, corruptf("frame at byte %d changes format", off)
	}
	s := f.Subframes[0].Samples
	if len(s) == 0 || len(s) != int(f.BlockSize) {
		return decoded{}, corruptf("frame at byte %d has %d samples, header says %d", off, len(s), f.BlockSize)
	}
	for _, v := range s {
		if v < -1<<15 || v >= 1<<15 {
			return decoded{}, corruptf("sample out of 16-bit range in frame at byte %d", off)
		}
	}
	start := d.pos
	d.pos += int64(len(s))
	if d.pos > d.max {
		return decoded{}, fmt.Errorf("%w: more than %d samples", ErrTooLong, d.max)
	}
	return decoded{offset: off, start: start, samples: s}, nil
}

// wrap classifies a library error: failures of the underlying reader pass
// through untouched, everything else is corruption.
func (d *decoder) wrap(what string, err error) error {
	if d.cr.err != nil {
		return d.cr.err
	}
	return corruptf("%s: %v", what, err)
}

// countReader counts bytes and remembers the first real read error, so that
// I/O failures can be told apart from malformed data.
type countReader struct {
	r   io.Reader
	n   int64
	err error
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && !errors.Is(err, io.EOF) && c.err == nil {
		c.err = err
	}
	return n, err
}

// recoverPanic converts a panic in the FLAC library into an ErrCorrupt error.
func recoverPanic(err *error) {
	if p := recover(); p != nil {
		*err = corruptf("decoder panic: %v", p)
	}
}
