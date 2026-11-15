package media

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// blockSize is the encoder's frame length in samples.
const blockSize = 4096

// EncodeFLAC encodes canonical s16le mono PCM to FLAC, reading exactly
// samples samples from pcm in blockSize-sample frames, so memory use is
// constant. dst must be seekable: STREAMINFO is rewritten at the end with the
// final sample count and MD5. On return dst is positioned at its end.
//
// samples must be at least MinSamples. A pcm that ends early yields an error
// matching ErrCorrupt.
func EncodeFLAC(dst io.WriteSeeker, pcm io.Reader, samples int64) error {
	return EncodeFLACContext(context.Background(), dst, pcm, samples)
}

// EncodeFLACContext is EncodeFLAC that checks ctx before every frame and
// returns ctx's error once it ends.
func EncodeFLACContext(ctx context.Context, dst io.WriteSeeker, pcm io.Reader, samples int64) error {
	if samples < MinSamples {
		return fmt.Errorf("%w: %d samples", ErrTooShort, samples)
	}
	out := &seekBuffer{bw: bufio.NewWriterSize(dst, 64<<10), ws: dst}
	info := &meta.StreamInfo{
		BlockSizeMin:  blockSize,
		BlockSizeMax:  blockSize,
		SampleRate:    SampleRate,
		NChannels:     Channels,
		BitsPerSample: BitsPerSample,
		NSamples:      uint64(samples),
	}
	enc, err := flac.NewEncoder(out, info)
	if err != nil {
		return fmt.Errorf("media: start FLAC encoder: %w", err)
	}

	sizes := blockSizes(samples)
	raw := make([]byte, blockSize*BytesPerSample+BytesPerSample*MinSamples)
	buf := make([]int32, 0, cap(raw)/BytesPerSample)
	sub := &frame.Subframe{}
	f := &frame.Frame{
		Header: frame.Header{
			HasFixedBlockSize: !sizes.variable,
			SampleRate:        SampleRate,
			Channels:          frame.ChannelsMono,
			BitsPerSample:     BitsPerSample,
		},
		Subframes: []*frame.Subframe{sub},
	}
	for rem := samples; rem > 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := sizes.take(rem)
		rem -= int64(n)
		b := raw[:n*BytesPerSample]
		if _, err := io.ReadFull(pcm, b); err != nil {
			return corruptf("PCM ended early: %v", err)
		}
		buf = buf[:n]
		for i := range buf {
			buf[i] = int32(int16(binary.LittleEndian.Uint16(b[i*BytesPerSample:])))
		}
		f.BlockSize = uint16(n)
		*sub = frame.Subframe{
			SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
			Samples:   buf,
			NSamples:  n,
		}
		if err := enc.WriteFrame(f); err != nil {
			return fmt.Errorf("media: encode FLAC frame: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("media: finish FLAC: %w", err)
	}
	if err := out.bw.Flush(); err != nil {
		return err
	}
	_, err = dst.Seek(0, io.SeekEnd)
	return err
}

// plan describes how a stream of n samples is cut into frames. Frames are
// blockSize long except the last. FLAC forbids frames under 16 samples in
// STREAMINFO, so when the remainder would be that short the last two frames
// share their samples and the stream is flagged variable-block-size.
type plan struct{ variable bool }

func blockSizes(n int64) plan {
	rem := n % blockSize
	return plan{variable: n > blockSize && rem != 0 && rem < MinSamples}
}

// take returns the length of the next frame when rem samples remain.
func (p plan) take(rem int64) int {
	switch {
	case rem <= blockSize:
		return int(rem)
	case p.variable && rem < blockSize+MinSamples:
		return int(rem / 2)
	}
	return blockSize
}

// seekBuffer buffers the encoder's many small writes while still letting the
// encoder seek back to rewrite STREAMINFO. It hides Close from the encoder.
type seekBuffer struct {
	bw *bufio.Writer
	ws io.WriteSeeker
}

func (s *seekBuffer) Write(p []byte) (int, error) { return s.bw.Write(p) }

func (s *seekBuffer) Seek(off int64, whence int) (int64, error) {
	if err := s.bw.Flush(); err != nil {
		return 0, err
	}
	return s.ws.Seek(off, whence)
}
