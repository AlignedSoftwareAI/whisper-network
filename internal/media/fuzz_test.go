package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"testing"
)

// TestMain silences the standard logger: mewkiz/flac writes a log line for
// some hostile frame headers, which floods fuzz workers' stderr.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

func FuzzWAV(f *testing.F) {
	f.Add(toWAV(speechLike(100, 1)))
	f.Add(riff(ck("LIST", []byte("abc")), goodFmt, pcm100))
	f.Add(riff(ck("fmt ", extBody(1, 16)), pcm100))
	f.Add(riff(goodFmt, chunk{"data", 400, make([]byte, 100)}))
	f.Add(riff(chunk{"junk", 0xFFFFFFF0, nil}))
	f.Fuzz(func(t *testing.T, in []byte) {
		info, err := ValidateWAV(bytes.NewReader(in), 1<<20)
		if err == nil && (info.Samples < MinSamples || int(info.Samples)*2+44 > len(in)+len(in)) {
			t.Fatalf("accepted %d samples from %d bytes", info.Samples, len(in))
		}
		if err != nil && !isMediaErr(err) {
			t.Fatalf("unclassified error: %v", err)
		}
	})
}

func FuzzFLAC(f *testing.F) {
	f.Add(encodeFLAC(f, speechLike(5000, 1)))
	f.Add(encodeFLAC(f, speechLike(4096+5, 2)))
	f.Add(encodeFLAC(f, make([]int16, 3000)))
	f.Add(encodeFLAC(f, noise(2000, 3)))
	f.Add([]byte("fLaC"))
	f.Fuzz(func(t *testing.T, in []byte) {
		info, err := ValidateFLAC(bytes.NewReader(in), 1<<20)
		if err != nil {
			if !isMediaErr(err) {
				t.Fatalf("unclassified error: %v", err)
			}
			return
		}
		// Whatever validates must analyze and serve identically.
		an, err := AnalyzeFLAC(context.Background(), bytes.NewReader(in), AnalyzeOptions{MaxSamples: 1 << 20})
		if err != nil || an.Samples != info.Samples {
			t.Fatalf("analyze disagrees with validate: %v", err)
		}
		var out bytes.Buffer
		whole := Range{0, info.Samples}
		if err := ServeRange(&out, bytes.NewReader(in), an.Index, whole); err != nil {
			t.Fatalf("serve of validated FLAC: %v", err)
		}
		if int64(out.Len()) != 44+info.Samples*2 {
			t.Fatalf("served %d bytes for %d samples", out.Len(), info.Samples)
		}
	})
}

func isMediaErr(err error) bool {
	for _, e := range []error{ErrCorrupt, ErrNotCanonical, ErrTooLong, ErrTooShort} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

func TestRecoverPanic(t *testing.T) {
	f := func() (err error) {
		defer recoverPanic(&err)
		panic("boom")
	}
	if err := f(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v", err)
	}
}
