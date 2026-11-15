package media

import (
	"bytes"
	"context"
	"io"
	"testing"
)

// benchSeconds is the audio length of benchmark fixtures. Throughput is
// reported as audio seconds per wall second via the "x-realtime" metric.
const benchSeconds = 600

func benchFixture(b *testing.B) (pcm []byte, fl []byte, an *Analysis) {
	b.Helper()
	s := speechLike(benchSeconds*SampleRate, 42)
	pcm = pcmBytes(s)
	fl = encodeFLAC(b, s)
	an, err := AnalyzeFLAC(context.Background(), bytes.NewReader(fl), AnalyzeOptions{})
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("fixture: %d s speech-like, PCM %d B, FLAC %d B (%.1f%%)",
		benchSeconds, len(pcm), len(fl), float64(len(fl))/float64(len(pcm))*100)
	return pcm, fl, an
}

func realtime(b *testing.B, seconds float64) {
	b.ReportMetric(seconds*float64(b.N)/b.Elapsed().Seconds(), "x-realtime")
}

func BenchmarkEncodeFLAC(b *testing.B) {
	pcm, _, _ := benchFixture(b)
	b.SetBytes(int64(len(pcm)))
	b.ResetTimer()
	for range b.N {
		var m memWS
		if err := EncodeFLAC(&m, bytes.NewReader(pcm), int64(len(pcm)/2)); err != nil {
			b.Fatal(err)
		}
	}
	realtime(b, benchSeconds)
}

func BenchmarkValidateFLAC(b *testing.B) {
	pcm, fl, _ := benchFixture(b)
	b.SetBytes(int64(len(pcm)))
	b.ResetTimer()
	for range b.N {
		if _, err := ValidateFLAC(bytes.NewReader(fl), 0); err != nil {
			b.Fatal(err)
		}
	}
	realtime(b, benchSeconds)
}

func BenchmarkAnalyzeFLAC(b *testing.B) {
	pcm, fl, _ := benchFixture(b)
	b.SetBytes(int64(len(pcm)))
	b.ResetTimer()
	for range b.N {
		if _, err := AnalyzeFLAC(context.Background(), bytes.NewReader(fl), AnalyzeOptions{}); err != nil {
			b.Fatal(err)
		}
	}
	realtime(b, benchSeconds)
}

func BenchmarkValidateWAV(b *testing.B) {
	pcm, _, _ := benchFixture(b)
	wav := append(func() []byte { h := WAVHeader(int64(len(pcm) / 2)); return h[:] }(), pcm...)
	b.SetBytes(int64(len(wav)))
	b.ResetTimer()
	for range b.N {
		if _, err := ValidateWAV(bytes.NewReader(wav), 0); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkServeRange serves a 10-minute range from the end of the file, the
// worst case for the unindexed path.
func BenchmarkServeRange(b *testing.B) {
	_, fl, an := benchFixture(b)
	rng := Range{Start: an.Samples - 120*SampleRate, End: an.Samples}
	for name, idx := range map[string]*Index{"indexed": an.Index, "unindexed": nil} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(rng.Len() * BytesPerSample)
			for range b.N {
				if err := ServeRange(io.Discard, bytes.NewReader(fl), idx, rng); err != nil {
					b.Fatal(err)
				}
			}
			realtime(b, 120)
		})
	}
}
