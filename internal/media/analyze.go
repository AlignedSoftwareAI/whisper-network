package media

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Analysis parameters.
const (
	// EnergyFrame is the RMS analysis frame: 100 ms.
	EnergyFrame = SampleRate / 10

	// DefaultTarget and DefaultWindow are the default split spacing and the
	// search half-width around each target.
	DefaultTarget = 60 * time.Minute
	DefaultWindow = 60 * time.Second

	// indexSpacing is the minimum sample distance between index points.
	indexSpacing = SampleRate

	// minTail keeps a cut at least this far from the end of the audio so the
	// last range is never a sliver.
	minTail = SampleRate
)

// AnalyzeOptions tunes AnalyzeFLAC. Zero values select the defaults.
type AnalyzeOptions struct {
	MaxSamples int64         // length limit; DefaultMaxSamples
	Target     time.Duration // spacing of cuts; DefaultTarget
	Window     time.Duration // cuts fall within +/- Window of a multiple of Target; DefaultWindow
	// MaxSamplesPerByte, if positive, bounds how well the stream may
	// compress: once more than a minute has decoded, a stream averaging more
	// samples per frame byte (metadata blocks are not counted) than this
	// fails with ErrNotCanonical. Real recordings average well under 10
	// samples/byte; only long runs of constant (digitally silent) frames
	// reach hundreds, and those let a few MB of input cost seconds of
	// decoding. Zero disables the check.
	MaxSamplesPerByte float64
}

// Analysis is the result of AnalyzeFLAC.
type Analysis struct {
	Info
	// Splits are contiguous ranges covering [0, Samples).
	Splits []Range
	// Index lets ServeRange seek without rescanning the file.
	Index *Index
}

// AnalyzeFLAC validates a canonical FLAC exactly as ValidateFLAC does, and in
// the same single pass computes 100 ms RMS energy to choose split points, and
// a seek index. Memory is one 8-byte energy value per 100 ms of audio plus one
// index point per second, both a few MB for 12 hours.
//
// It checks ctx before every frame and returns ctx's error once it ends.
func AnalyzeFLAC(ctx context.Context, r io.Reader, opt AnalyzeOptions) (*Analysis, error) {
	target, window := opt.Target, opt.Window
	if target <= 0 {
		target = DefaultTarget
	}
	if window <= 0 {
		window = DefaultWindow
	}
	if 2*window >= target {
		return nil, fmt.Errorf("media: window %v must be under half the target %v", window, target)
	}

	var (
		energy []uint64
		sum    uint64
		fill   int
		idx    Index
	)
	lim := scanLimits{maxSamples: opt.MaxSamples, maxPerByte: opt.MaxSamplesPerByte}
	info, err := scan(ctx, r, lim, func(fr decoded) {
		if n := len(idx.Points); n == 0 || fr.start-idx.Points[n-1].Sample >= indexSpacing {
			idx.Points = append(idx.Points, SeekPoint{Sample: fr.start, Offset: fr.offset})
		}
		for s := fr.samples; len(s) > 0; {
			n := min(EnergyFrame-fill, len(s))
			for _, v := range s[:n] {
				sum += uint64(int64(v) * int64(v))
			}
			s = s[n:]
			if fill += n; fill == EnergyFrame {
				energy = append(energy, sum)
				sum, fill = 0, 0
			}
		}
	})
	if err != nil {
		return nil, err
	}
	idx.Samples = info.Samples
	return &Analysis{
		Info:   info,
		Splits: SplitPoints(energy, info.Samples, durSamples(target), durSamples(window)),
		Index:  &idx,
	}, nil
}

func durSamples(d time.Duration) int64 {
	return int64(d) * SampleRate / int64(time.Second)
}

// SplitPoints chooses cut points given the sum of squares of each
// EnergyFrame-sample frame of a total-sample signal (a trailing partial
// frame is omitted). Cuts go near each multiple of target, at the centre of
// the quietest frame lying wholly within +/- window of it; ties prefer the
// frame nearest the target. A cut is made only when its whole window fits in
// the audio, so inputs shorter than target+window yield one range. The result
// is contiguous and covers [0, total). It requires 2*window < target.
func SplitPoints(energy []uint64, total, target, window int64) []Range {
	var out []Range
	start := int64(0)
	for c := target; c+window <= total; c += target {
		lo := max((c-window+EnergyFrame-1)/EnergyFrame, 0)
		hi := min((c+window)/EnergyFrame, int64(len(energy))) // exclusive
		best, bestDist := int64(-1), int64(0)
		for f := lo; f < hi; f++ {
			if f*EnergyFrame+EnergyFrame/2 > total-minTail {
				break
			}
			dist := abs(f*EnergyFrame + EnergyFrame/2 - c)
			if best < 0 || energy[f] < energy[best] || (energy[f] == energy[best] && dist < bestDist) {
				best, bestDist = f, dist
			}
		}
		if best < 0 {
			continue
		}
		cut := best*EnergyFrame + EnergyFrame/2
		out = append(out, Range{Start: start, End: cut})
		start = cut
	}
	return append(out, Range{Start: start, End: total})
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
