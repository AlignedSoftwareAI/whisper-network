package media

import (
	"bytes"
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withSilences returns loud noise with 1 s of digital silence centred on each
// given sample position.
func withSilences(n int, centres ...int) []int16 {
	rng := rand.New(rand.NewPCG(9, 9))
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(rng.IntN(8000) - 4000)
	}
	for _, c := range centres {
		clear(out[c-SampleRate/2 : c+SampleRate/2])
	}
	return out
}

func TestAnalyzeSplitsLandInSilence(t *testing.T) {
	// target 20 s, window 4 s; silences planted near, not on, the targets.
	const sec = SampleRate
	n := 55 * sec
	silences := []int{19*sec + sec/3, 41 * sec}
	pcm := withSilences(n, silences...)
	an, err := AnalyzeFLAC(context.Background(), bytes.NewReader(encodeFLAC(t, pcm)),
		AnalyzeOptions{Target: 20 * time.Second, Window: 4 * time.Second})
	require.NoError(t, err)

	require.Len(t, an.Splits, 3)
	for i, c := range silences {
		cut := an.Splits[i].End
		assert.InDelta(t, c, cut, sec/2, "cut %d", i)
		assert.Zero(t, pcm[cut], "cut %d lands in silence", i)
	}
	assertCovers(t, an.Splits, int64(n))
	assert.Equal(t, int64(n), an.Samples)
}

func TestSplitPointsShortInput(t *testing.T) {
	target, window := int64(1000*EnergyFrame), int64(100*EnergyFrame)
	total := target + window - 1
	got := SplitPoints(make([]uint64, total/EnergyFrame), total, target, window)
	assert.Equal(t, []Range{{0, total}}, got)

	total = target + window
	got = SplitPoints(make([]uint64, total/EnergyFrame), total, target, window)
	assert.Len(t, got, 2, "exactly target+window gets a cut")
	assertCovers(t, got, total)
}

func TestSplitPointsProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 5))
	target, window := int64(600*EnergyFrame), int64(60*EnergyFrame)
	for range 200 {
		frames := rng.IntN(5000) + 1
		total := int64(frames*EnergyFrame + rng.IntN(EnergyFrame))
		energy := make([]uint64, frames)
		for i := range energy {
			energy[i] = uint64(rng.IntN(100))
		}
		got := SplitPoints(energy, total, target, window)
		assertCovers(t, got, total)
		for i, r := range got[:len(got)-1] {
			c := int64(i+1) * target
			assert.LessOrEqual(t, abs(r.End-c), window, "cut within window of target")
		}
	}
}

func TestSplitPointsTieBreaksNearTarget(t *testing.T) {
	target, window := int64(100*EnergyFrame), int64(10*EnergyFrame)
	energy := make([]uint64, 130)
	got := SplitPoints(energy, 130*EnergyFrame, target, window)
	require.Len(t, got, 2)
	assert.InDelta(t, target, got[0].End, EnergyFrame, "all-equal energy cuts at the target")
}

func TestAnalyzeRejectsBadWindow(t *testing.T) {
	_, err := AnalyzeFLAC(context.Background(), bytes.NewReader(nil), AnalyzeOptions{Target: time.Minute, Window: time.Minute})
	require.Error(t, err)
}

func TestAnalyzeValidates(t *testing.T) {
	fl := encodeFLAC(t, speechLike(20000, 2))
	fl[len(fl)-5] ^= 0xFF
	_, err := AnalyzeFLAC(context.Background(), bytes.NewReader(fl), AnalyzeOptions{})
	require.ErrorIs(t, err, ErrCorrupt)
}

func TestAnalyzeIndexMonotonic(t *testing.T) {
	an, err := AnalyzeFLAC(context.Background(), bytes.NewReader(encodeFLAC(t, speechLike(16000*6, 3))), AnalyzeOptions{})
	require.NoError(t, err)
	pts := an.Index.Points
	require.NotEmpty(t, pts)
	assert.Zero(t, pts[0].Sample)
	for i := 1; i < len(pts); i++ {
		assert.Greater(t, pts[i].Sample, pts[i-1].Sample)
		assert.Greater(t, pts[i].Offset, pts[i-1].Offset)
	}
}

func assertCovers(t *testing.T, rs []Range, total int64) {
	t.Helper()
	require.NotEmpty(t, rs)
	assert.Zero(t, rs[0].Start)
	assert.Equal(t, total, rs[len(rs)-1].End)
	for i, r := range rs {
		assert.Greater(t, r.End, r.Start, "range %d non-empty", i)
		if i > 0 {
			assert.Equal(t, rs[i-1].End, r.Start, "range %d contiguous", i)
		}
	}
}
