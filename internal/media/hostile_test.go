package media

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// constantFLAC encodes samples of digital silence as constant subframes of
// blockSize samples: a valid canonical FLAC of a dozen bytes per frame,
// whose decode costs as much as any other audio of that length.
// Any extra metadata blocks are written after STREAMINFO.
func constantFLAC(t testing.TB, samples int64, blockSize int, blocks ...*meta.Block) []byte {
	t.Helper()
	var m memWS
	enc, err := flac.NewEncoder(&m, &meta.StreamInfo{
		BlockSizeMin: uint16(blockSize), BlockSizeMax: uint16(blockSize),
		SampleRate: SampleRate, NChannels: Channels, BitsPerSample: BitsPerSample, NSamples: uint64(samples),
	}, blocks...)
	require.NoError(t, err)
	buf := make([]int32, blockSize)
	sub := &frame.Subframe{SubHeader: frame.SubHeader{Pred: frame.PredConstant}}
	f := &frame.Frame{
		Header: frame.Header{HasFixedBlockSize: true, SampleRate: SampleRate, Channels: frame.ChannelsMono,
			BitsPerSample: BitsPerSample},
		Subframes: []*frame.Subframe{sub},
	}
	for rem := samples; rem > 0; {
		n := int(min(rem, int64(blockSize)))
		rem -= int64(n)
		f.BlockSize = uint16(n)
		sub.Samples, sub.NSamples = buf[:n], n
		require.NoError(t, enc.WriteFrame(f))
	}
	require.NoError(t, enc.Close())
	return m.buf
}

// TestScanStopsWithinAFrameOfCancel: the original issue was that a cancelled
// analysis of an in-memory (or well-buffered) constant-frame FLAC kept
// decoding for seconds, because ctx was only checked on Read. Every scan
// loop now checks ctx per frame.
func TestScanStopsWithinAFrameOfCancel(t *testing.T) {
	fl := constantFLAC(t, 30*60*SampleRate, 4096) // 30 min of audio in ~90 KB

	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := AnalyzeFLAC(ctx, bytes.NewReader(fl), AnalyzeOptions{})
		require.ErrorIs(t, err, context.Canceled)
		_, err = ValidateFLACContext(ctx, bytes.NewReader(fl), 0)
		require.ErrorIs(t, err, context.Canceled)
		var m memWS
		err = EncodeFLACContext(ctx, &m, bytes.NewReader(make([]byte, 2*SampleRate)), SampleRate)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("cancelled midway", func(t *testing.T) {
		full := time.Now()
		_, err := AnalyzeFLAC(context.Background(), bytes.NewReader(fl), AnalyzeOptions{})
		require.NoError(t, err)
		whole := time.Since(full)

		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(whole/20, cancel)
		start := time.Now()
		_, err = AnalyzeFLAC(ctx, bytes.NewReader(fl), AnalyzeOptions{})
		took := time.Since(start)
		require.ErrorIs(t, err, context.Canceled)
		assert.Less(t, took, whole/2, "a cancelled scan must stop long before the full decode (%v)", whole)
	})
}

func TestMaxSamplesPerByte(t *testing.T) {
	const limit = 32
	silent := constantFLAC(t, 3*60*SampleRate, 4096)
	ratio := float64(3*60*SampleRate) / float64(len(silent))
	require.Greater(t, ratio, 256.0, "fixture: constant frames compress far beyond the limit")
	t.Logf("constant 4096-sample frames: %.0f samples/byte", ratio)

	_, err := AnalyzeFLAC(context.Background(), bytes.NewReader(silent), AnalyzeOptions{MaxSamplesPerByte: limit})
	require.ErrorIs(t, err, ErrNotCanonical, "routed to the prepare path, not decoded on the server")

	_, err = AnalyzeFLAC(context.Background(), bytes.NewReader(silent), AnalyzeOptions{})
	require.NoError(t, err, "no limit: still a valid canonical FLAC")

	short := constantFLAC(t, 30*SampleRate, 4096)
	_, err = AnalyzeFLAC(context.Background(), bytes.NewReader(short), AnalyzeOptions{MaxSamplesPerByte: limit})
	require.NoError(t, err, "short silent clips are within the grace period")

	speech := encodeFLAC(t, speechLike(2*60*SampleRate, 7))
	_, err = AnalyzeFLAC(context.Background(), bytes.NewReader(speech), AnalyzeOptions{MaxSamplesPerByte: limit})
	require.NoError(t, err, "real audio is nowhere near the limit")

	// Speech followed by a long stretch of digital silence, as our encoder
	// writes it, averages a few samples per byte: still the fast path.
	gappy := encodeFLAC(t, append(speechLike(30*SampleRate, 3), make([]int16, 2*60*SampleRate)...))
	_, err = AnalyzeFLAC(context.Background(), bytes.NewReader(gappy), AnalyzeOptions{MaxSamplesPerByte: limit})
	require.NoError(t, err, "speech with long silences stays on the fast path")
}

// TestMaxSamplesPerByteIgnoresMetadata reproduces the padded bomb: constant
// frames behind a PADDING block big enough that the whole file's ratio is
// under the limit. Metadata costs nothing to decode, so only frame bytes
// count.
func TestMaxSamplesPerByteIgnoresMetadata(t *testing.T) {
	const limit, samples = 32, 3 * 60 * SampleRate
	pad := &meta.Block{Header: meta.Header{Type: meta.TypePadding, Length: samples / (limit - 2)}}
	padded := constantFLAC(t, samples, 4096, pad)
	require.Less(t, float64(samples)/float64(len(padded)), float64(limit),
		"fixture: counting the padding brings the ratio under the limit")

	_, err := AnalyzeFLAC(context.Background(), bytes.NewReader(padded), AnalyzeOptions{MaxSamplesPerByte: limit})
	require.ErrorIs(t, err, ErrNotCanonical, "padding does not buy the fast path")
	_, err = AnalyzeFLAC(context.Background(), bytes.NewReader(padded), AnalyzeOptions{})
	require.NoError(t, err, "still a valid canonical FLAC")
}
