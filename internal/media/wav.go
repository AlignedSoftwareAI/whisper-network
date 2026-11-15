package media

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Bounds on what the WAV parser will tolerate before the data chunk.
const (
	maxFmtChunk  = 1024     // largest fmt chunk read; real ones are 16, 18 or 40 bytes
	maxSkipBytes = 16 << 20 // total bytes of unknown chunks skipped (LIST, id3, ...)

	waveFormatPCM        = 0x0001
	waveFormatExtensible = 0xFFFE
)

// pcmSubformat is the KSDATAFORMAT_SUBTYPE_PCM GUID tail (bytes 2..16); the
// first two bytes carry the format tag.
var pcmSubformatTail = []byte{0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}

// WAV is a validated canonical WAV stream positioned at its sample data.
type WAV struct {
	Info
	// PCM yields exactly Samples*2 bytes of s16le audio. A stream that ends
	// early fails with an error matching ErrCorrupt.
	PCM io.Reader
}

// OpenWAV parses the headers of a streamed RIFF/WAVE file and returns a
// reader over its sample data, without buffering the audio.
//
// The error matches ErrNotCanonical for a well-formed WAV that is not 16 kHz
// mono 16-bit PCM (including extensible, float, compressed and RF64 files),
// ErrCorrupt for malformed headers, and ErrTooLong or ErrTooShort for a
// canonical file outside the length limits. maxSamples <= 0 selects
// DefaultMaxSamples. Chunks after the data chunk are never read.
func OpenWAV(r io.Reader, maxSamples int64) (*WAV, error) {
	if maxSamples <= 0 {
		maxSamples = DefaultMaxSamples
	}
	br := bufio.NewReaderSize(r, 64<<10)

	var riff [12]byte
	if _, err := io.ReadFull(br, riff[:]); err != nil {
		return nil, corruptf("short RIFF header: %v", err)
	}
	if !bytes.Equal(riff[8:12], []byte("WAVE")) {
		return nil, corruptf("not a WAVE file")
	}
	switch string(riff[:4]) {
	case "RIFF":
	case "RF64", "BW64":
		return nil, fmt.Errorf("%w: %s container", ErrNotCanonical, riff[:4])
	default:
		return nil, corruptf("not a RIFF file")
	}

	var (
		haveFmt bool
		skipped int64
	)
	for {
		var ch [8]byte
		if _, err := io.ReadFull(br, ch[:]); err != nil {
			return nil, corruptf("no data chunk: %v", err)
		}
		id := string(ch[:4])
		size := int64(binary.LittleEndian.Uint32(ch[4:]))

		switch id {
		case "fmt ":
			if haveFmt {
				return nil, corruptf("duplicate fmt chunk")
			}
			if err := readFmt(br, size); err != nil {
				return nil, err
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return nil, corruptf("data chunk before fmt chunk")
			}
			return openData(br, size, maxSamples)
		default:
			skipped += size
			if skipped > maxSkipBytes {
				return nil, corruptf("unreasonable %q chunk of %d bytes", id, size)
			}
			if err := discard(br, size); err != nil {
				return nil, corruptf("truncated %q chunk: %v", id, err)
			}
		}
		if size%2 == 1 { // chunks are word aligned
			if err := discard(br, 1); err != nil {
				return nil, corruptf("missing padding byte after %q chunk", id)
			}
		}
	}
}

// ValidateWAV fully reads a WAV stream, confirming its headers are canonical
// and that all declared sample data is present. See OpenWAV for the errors.
func ValidateWAV(r io.Reader, maxSamples int64) (Info, error) {
	w, err := OpenWAV(r, maxSamples)
	if err != nil {
		return Info{}, err
	}
	if _, err := io.Copy(io.Discard, w.PCM); err != nil {
		return Info{}, err
	}
	return w.Info, nil
}

func readFmt(br *bufio.Reader, size int64) error {
	if size < 16 || size > maxFmtChunk {
		return corruptf("fmt chunk of %d bytes", size)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(br, b); err != nil {
		return corruptf("truncated fmt chunk: %v", err)
	}
	tag := binary.LittleEndian.Uint16(b[0:])
	channels := binary.LittleEndian.Uint16(b[2:])
	rate := binary.LittleEndian.Uint32(b[4:])
	byteRate := binary.LittleEndian.Uint32(b[8:])
	align := binary.LittleEndian.Uint16(b[12:])
	bits := binary.LittleEndian.Uint16(b[14:])

	if tag == waveFormatExtensible {
		if size < 40 || binary.LittleEndian.Uint16(b[16:]) < 22 {
			return corruptf("short extensible fmt chunk")
		}
		validBits := binary.LittleEndian.Uint16(b[18:])
		sub := b[24:40]
		if !bytes.Equal(sub[2:], pcmSubformatTail) {
			return fmt.Errorf("%w: extensible subformat is not PCM", ErrNotCanonical)
		}
		tag = binary.LittleEndian.Uint16(sub)
		if validBits != 0 && validBits != bits {
			return fmt.Errorf("%w: %d valid bits in a %d-bit container", ErrNotCanonical, validBits, bits)
		}
	}
	if channels == 0 || bits == 0 || rate == 0 {
		return corruptf("zero channels, rate or bit depth")
	}
	if tag != waveFormatPCM {
		return fmt.Errorf("%w: format tag %#x", ErrNotCanonical, tag)
	}
	if rate != SampleRate || channels != Channels || bits != BitsPerSample {
		return fmt.Errorf("%w: %d Hz, %d ch, %d-bit", ErrNotCanonical, rate, channels, bits)
	}
	if int(align) != Channels*BytesPerSample || byteRate != SampleRate*uint32(align) {
		return corruptf("inconsistent block align %d / byte rate %d", align, byteRate)
	}
	return nil
}

func openData(br *bufio.Reader, size, maxSamples int64) (*WAV, error) {
	if size%BytesPerSample != 0 {
		return nil, corruptf("data size %d is not a multiple of the block align", size)
	}
	n := size / BytesPerSample
	switch {
	case n > maxSamples:
		return nil, fmt.Errorf("%w: data chunk declares %d samples, limit %d", ErrTooLong, n, maxSamples)
	case n < MinSamples:
		return nil, fmt.Errorf("%w: %d samples", ErrTooShort, n)
	}
	return &WAV{Info: Info{Samples: n}, PCM: &pcmReader{r: br, left: size}}, nil
}

// pcmReader limits the data chunk and reports early EOF as corruption.
type pcmReader struct {
	r    io.Reader
	left int64
}

func (p *pcmReader) Read(b []byte) (int, error) {
	if p.left == 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > p.left {
		b = b[:p.left]
	}
	n, err := p.r.Read(b)
	p.left -= int64(n)
	if p.left == 0 && (err == nil || errors.Is(err, io.EOF)) {
		return n, io.EOF
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return n, corruptf("data chunk truncated, %d bytes missing", p.left)
	}
	return n, err
}

func discard(r io.Reader, n int64) error {
	_, err := io.CopyN(io.Discard, r, n)
	return err
}

func corruptf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, args...)...)
}
