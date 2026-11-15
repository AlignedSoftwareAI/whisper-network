package media

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serve(t *testing.T, fl []byte, idx *Index, r Range) ([]int16, error) {
	t.Helper()
	var out bytes.Buffer
	err := ServeRange(&out, bytes.NewReader(fl), idx, r)
	if err != nil {
		return nil, err
	}
	return decodeWAV(t, out.Bytes()), nil
}

func TestServeRangeExact(t *testing.T) {
	const n = 3*4096 + 1000
	pcm := speechLike(n, 11)
	fl := encodeFLAC(t, pcm)
	an, err := AnalyzeFLAC(context.Background(), bytes.NewReader(fl), AnalyzeOptions{})
	require.NoError(t, err)

	ranges := []Range{
		{0, n},                // everything
		{0, 1},                // first sample
		{n - 1, n},            // last sample
		{n - 1000, n},         // the short final frame
		{n - 1500, n - 500},   // straddles final boundary
		{4095, 4097},          // straddles a frame boundary
		{4096, 8192},          // exactly one frame
		{100, 4000},           // inside one frame
		{4000, 3*4096 + 50},   // several frames, partial at both ends
		{8191, 8192},          // last sample of a frame
		{8192, 8193},          // first sample of a frame
		{5, 6},                // single sample
		{12288, 12288 + 1000}, // exactly the final frame
	}
	for _, r := range ranges {
		for name, idx := range map[string]*Index{"no index": nil, "index": an.Index} {
			t.Run(name, func(t *testing.T) {
				got, err := serve(t, fl, idx, r)
				require.NoError(t, err, "%v", r)
				require.Equal(t, pcm[r.Start:r.End], got, "%v", r)
			})
		}
	}
}

func TestServeRangeVariableBlocks(t *testing.T) {
	// 4096+5 samples forces the variable-block-size plan.
	n := 4096 + 5
	pcm := speechLike(n, 12)
	fl := encodeFLAC(t, pcm)
	an, err := AnalyzeFLAC(context.Background(), bytes.NewReader(fl), AnalyzeOptions{})
	require.NoError(t, err)
	for _, r := range []Range{{0, int64(n)}, {2000, 2100}, {int64(n) - 3, int64(n)}, {2040, 2060}} {
		got, err := serve(t, fl, an.Index, r)
		require.NoError(t, err)
		require.Equal(t, pcm[r.Start:r.End], got, "%v", r)
	}
}

func TestServeRangeManyFramesWithIndex(t *testing.T) {
	// Longer than several index spacings so seeking skips real distance.
	n := 16000*10 + 77
	pcm := speechLike(n, 13)
	fl := encodeFLAC(t, pcm)
	an, err := AnalyzeFLAC(context.Background(), bytes.NewReader(fl), AnalyzeOptions{})
	require.NoError(t, err)
	require.Greater(t, len(an.Index.Points), 5)
	for _, r := range []Range{{16000*7 + 3, 16000*7 + 9000}, {int64(n) - 5000, int64(n)}, {50000, 50001}} {
		got, err := serve(t, fl, an.Index, r)
		require.NoError(t, err)
		require.Equal(t, pcm[r.Start:r.End], got)
	}
}

func TestServeRangeBounds(t *testing.T) {
	pcm := speechLike(5000, 14)
	fl := encodeFLAC(t, pcm)
	for _, r := range []Range{{-1, 10}, {10, 10}, {20, 10}, {0, 5001}, {5000, 5001}} {
		var out bytes.Buffer
		err := ServeRange(&out, bytes.NewReader(fl), nil, r)
		require.ErrorIs(t, err, ErrRange, "%v", r)
		assert.Zero(t, out.Len(), "no header on a rejected range")
	}
}

func TestServeRangeCorrupt(t *testing.T) {
	fl := encodeFLAC(t, speechLike(20000, 15))
	bad := bytes.Clone(fl)
	bad[len(bad)/2] ^= 0xFF
	var out bytes.Buffer
	err := ServeRange(&out, bytes.NewReader(bad), nil, Range{0, 20000})
	require.ErrorIs(t, err, ErrCorrupt)
}

func TestServeRangeWriteError(t *testing.T) {
	fl := encodeFLAC(t, speechLike(20000, 16))
	boom := errors.New("client went away")
	err := ServeRange(failWriter{boom}, bytes.NewReader(fl), nil, Range{0, 100})
	require.ErrorIs(t, err, boom)
}

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

func TestWAVHeaderIsCanonical(t *testing.T) {
	h := WAVHeader(1000)
	assert.Equal(t, 44, len(h))
	w, err := OpenWAV(bytes.NewReader(append(h[:], make([]byte, 2000)...)), 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), w.Samples)
}
