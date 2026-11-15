package media

import (
	"bytes"
	"crypto/md5"
	"io"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleCounts straddle every frame-plan case: tiny, exact multiples,
// remainders under 16 (which force variable block sizes), and odd sizes.
var sampleCounts = []int{16, 17, 100, 4095, 4096, 4097, 4096 + 15, 4096 + 16, 3*4096 + 1, 3*4096 + 5, 10000, 50001}

func TestEncodeValidateRoundTrip(t *testing.T) {
	for _, n := range sampleCounts {
		t.Run("", func(t *testing.T) {
			pcm := speechLike(n, uint64(n))
			fl := encodeFLAC(t, pcm)

			info, err := ValidateFLAC(bytes.NewReader(fl), 0)
			require.NoError(t, err)
			assert.Equal(t, int64(n), info.Samples)

			st, err := flac.New(bytes.NewReader(fl))
			require.NoError(t, err)
			assert.Equal(t, uint64(n), st.Info.NSamples)
			assert.Equal(t, md5.Sum(pcmBytes(pcm)), st.Info.MD5sum)
			assert.GreaterOrEqual(t, st.Info.BlockSizeMin, uint16(16))
		})
	}
}

func TestEncodeErrors(t *testing.T) {
	var m memWS
	require.ErrorIs(t, EncodeFLAC(&m, bytes.NewReader(make([]byte, 20)), 10), ErrTooShort)
	m = memWS{}
	require.ErrorIs(t, EncodeFLAC(&m, bytes.NewReader(make([]byte, 100)), 1000), ErrCorrupt)
}

func TestEncodeCompression(t *testing.T) {
	pcm := speechLike(16000*20, 7)
	fl := encodeFLAC(t, pcm)
	ratio := float64(len(fl)) / float64(len(pcm)*2)
	t.Logf("speech-like 20 s: %d -> %d bytes (%.1f%% of PCM)", len(pcm)*2, len(fl), ratio*100)
	assert.Less(t, ratio, 0.8)

	sil := encodeFLAC(t, make([]int16, 16000*20))
	assert.Less(t, len(sil), 2000, "digital silence should be near free")

	nz := noise(16000*5, 3)
	fn := encodeFLAC(t, nz)
	t.Logf("white noise 5 s: %.1f%% of PCM", float64(len(fn))/float64(len(nz)*2)*100)
	assert.Less(t, len(fn), len(nz)*2+len(nz)/50, "incompressible input must stay near verbatim size")
}

func TestValidateFLACLimits(t *testing.T) {
	fl := encodeFLAC(t, speechLike(20000, 1))
	_, err := ValidateFLAC(bytes.NewReader(fl), 19999)
	require.ErrorIs(t, err, ErrTooLong)
	_, err = ValidateFLAC(bytes.NewReader(fl), 20000)
	require.NoError(t, err)
}

// encodeOther encodes a valid FLAC in a non-canonical format.
func encodeOther(t *testing.T, rate uint32, channels int, bits uint8) []byte {
	t.Helper()
	var m memWS
	info := &meta.StreamInfo{BlockSizeMin: 64, BlockSizeMax: 64, SampleRate: rate,
		NChannels: uint8(channels), BitsPerSample: bits}
	enc, err := flac.NewEncoder(&m, info)
	require.NoError(t, err)
	ch := frame.ChannelsMono
	if channels == 2 {
		ch = frame.ChannelsLR
	}
	for range 4 {
		f := &frame.Frame{Header: frame.Header{HasFixedBlockSize: true, BlockSize: 64,
			SampleRate: rate, Channels: ch, BitsPerSample: bits}}
		for range channels {
			f.Subframes = append(f.Subframes, &frame.Subframe{
				SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
				Samples:   make([]int32, 64), NSamples: 64})
		}
		require.NoError(t, enc.WriteFrame(f))
	}
	require.NoError(t, enc.Close())
	return m.buf
}

func TestValidateFLACNotCanonical(t *testing.T) {
	tests := map[string][]byte{
		"stereo":  encodeOther(t, 16000, 2, 16),
		"44.1k":   encodeOther(t, 44100, 1, 16),
		"24-bit":  encodeOther(t, 16000, 1, 24),
		"8k mono": encodeOther(t, 8000, 1, 16),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateFLAC(bytes.NewReader(in), 0)
			require.ErrorIs(t, err, ErrNotCanonical)
		})
	}
}

func TestValidateFLACCorrupt(t *testing.T) {
	good := encodeFLAC(t, speechLike(30000, 5))
	_, err := ValidateFLAC(bytes.NewReader(good), 0)
	require.NoError(t, err)

	t.Run("flipped bytes", func(t *testing.T) {
		// Every byte after STREAMINFO is covered by a frame CRC or the MD5.
		for i := 42; i < len(good); i += 97 {
			bad := bytes.Clone(good)
			bad[i] ^= 0x10
			_, err := ValidateFLAC(bytes.NewReader(bad), 0)
			require.ErrorIs(t, err, ErrCorrupt, "byte %d", i)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		for _, n := range []int{0, 3, 4, 20, 42, 43, 100, len(good) / 2, len(good) - 1} {
			_, err := ValidateFLAC(bytes.NewReader(good[:n]), 0)
			require.Error(t, err, "len %d", n)
		}
	})
	t.Run("md5 mismatch", func(t *testing.T) {
		bad := bytes.Clone(good)
		bad[8+18+1] ^= 1 // inside the MD5 field of STREAMINFO
		_, err := ValidateFLAC(bytes.NewReader(bad), 0)
		require.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "MD5")
	})
	t.Run("sample count mismatch", func(t *testing.T) {
		bad := bytes.Clone(good)
		bad[8+17]++ // low byte of the NSamples field
		_, err := ValidateFLAC(bytes.NewReader(bad), 0)
		require.ErrorIs(t, err, ErrCorrupt)
	})
	t.Run("trailing garbage", func(t *testing.T) {
		_, err := ValidateFLAC(bytes.NewReader(append(bytes.Clone(good), 1, 2, 3, 4, 5)), 0)
		require.ErrorIs(t, err, ErrCorrupt)
	})
	t.Run("not flac", func(t *testing.T) {
		_, err := ValidateFLAC(bytes.NewReader(toWAV(make([]int16, 100))), 0)
		require.ErrorIs(t, err, ErrCorrupt)
	})
}

func TestValidateFLACReaderError(t *testing.T) {
	good := encodeFLAC(t, speechLike(30000, 5))
	r := io.MultiReader(bytes.NewReader(good[:200]), errReader{io.ErrClosedPipe})
	_, err := ValidateFLAC(r, 0)
	require.ErrorIs(t, err, io.ErrClosedPipe)
	require.NotErrorIs(t, err, ErrCorrupt)
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
