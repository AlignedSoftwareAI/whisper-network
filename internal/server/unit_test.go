package server

import (
	"encoding/json"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/blobs"
	"github.com/deweysasser/whisper-network/internal/models"
	"github.com/deweysasser/whisper-network/internal/store"
)

func TestFormatTimestamp(t *testing.T) {
	t.Parallel()
	for ms, want := range map[int64]string{
		0: "00:00.00", 9: "00:00.00", 10: "00:00.01", 4120: "00:04.12", 59_999: "00:59.99",
		60_000: "01:00.00", 3_672_400: "61:12.40", 12 * 3_600_000: "720:00.00",
	} {
		assert.Equal(t, want, formatTimestamp(ms), "%d ms", ms)
	}
}

func TestBuildTranscript(t *testing.T) {
	t.Parallel()
	job := &store.Job{ID: "0193-job", Model: "small.en", Language: "en", Source: "https://x.example/a;b\nc"}
	res := func(text, vad string, segs ...api.Segment) json.RawMessage {
		b, _ := json.Marshal(storedResult{Model: "small.en", VAD: vad, Language: "en", Segments: segs})
		return b
	}
	outs := &store.JobOutputs{
		Prepare: &store.TaskOutput{EngineVersion: "7.1.1"},
		Transcribe: []store.TaskOutput{
			{Seq: 1, StartSample: 0, EndSample: 16000 * 60, EngineVersion: "whisper.cpp 1.9.5", WorkerID: "w1",
				Result: res("", "silero-v5.1.2", api.Segment{StartMS: 0, EndMS: 1000, Text: "  hello\n world "},
					api.Segment{StartMS: 1000, EndMS: 1500, Text: "   "})},
			{Seq: 2, StartSample: 16000 * 3672, EndSample: 16000 * 4000, EngineVersion: "whisper.cpp 1.9.6", WorkerID: "w2",
				Result: res("", "silero-v5.1.2", api.Segment{StartMS: 400, EndMS: 900, Text: "later"})},
			{Seq: 3, StartSample: 16000 * 4000, EndSample: 16000 * 4100, EngineVersion: "whisper.cpp 1.9.5", WorkerID: "w1",
				Result: res("", "silero-v5.1.2")},
		},
	}
	now := time.Date(2026, 11, 15, 14, 3, 11, 500, time.UTC)
	segs, prov, err := buildTranscript(job, outs, now)
	require.NoError(t, err)
	assert.Equal(t, "transcript-source: whisper; whisper.cpp=1.9.5,1.9.6; model=small.en; vad=silero-v5.1.2; "+
		"language=en; transcribed=2026-11-15T14:03:11Z; job=0193-job; source=https://x.example/a%3Bb c; "+
		"ffmpeg=7.1.1; tasks=3; workers=2", prov)
	assert.Equal(t, []api.Segment{
		{StartMS: 0, EndMS: 1000, Text: "hello world"},
		{StartMS: 3_672_400, EndMS: 3_672_900, Text: "later"},
	}, segs, "absolute times, whitespace collapsed, empty segments dropped")

	// Fast path, no VAD, no source.
	job.Source = ""
	outs.Prepare = nil
	for i := range outs.Transcribe {
		outs.Transcribe[i].Result = res("", "")
	}
	_, prov, err = buildTranscript(job, outs, now)
	require.NoError(t, err)
	assert.Equal(t, "transcript-source: whisper; whisper.cpp=1.9.5,1.9.6; model=small.en; vad=none; "+
		"language=en; transcribed=2026-11-15T14:03:11Z; job=0193-job; tasks=3; workers=2", prov)
}

func TestValidateTranscribe(t *testing.T) {
	t.Parallel()
	small, err := models.Lookup("small.en")
	require.NoError(t, err)
	s := &Server{lookupModel: models.Lookup}
	lease := &store.Lease{Model: "small.en", StartSample: 16000, EndSample: 16000 * 11} // 10 s
	ok := func() api.TranscribeResult {
		return api.TranscribeResult{Model: "small.en", ModelSHA256: small.SHA256, ComputeMS: 5,
			Segments: []api.Segment{{StartMS: 0, EndMS: 4000, Text: "a"}, {StartMS: 4000, EndMS: 10_400, Text: "b"}}}
	}
	r := ok()
	require.NoError(t, s.validateTranscribe(&r, lease))
	assert.Equal(t, int64(10_000), r.Segments[1].EndMS, "small overrun clamped to the task end")

	bad := map[string]func(*api.TranscribeResult){
		"wrong model":        func(r *api.TranscribeResult) { r.Model = "large-v3" },
		"wrong sha":          func(r *api.TranscribeResult) { r.ModelSHA256 = "beef" },
		"negative compute":   func(r *api.TranscribeResult) { r.ComputeMS = -1 },
		"negative start":     func(r *api.TranscribeResult) { r.Segments[0].StartMS = -5 },
		"end before start":   func(r *api.TranscribeResult) { r.Segments[0].EndMS = -1 },
		"not monotonic":      func(r *api.TranscribeResult) { r.Segments[1].StartMS = 100; r.Segments[0].StartMS = 200 },
		"beyond end":         func(r *api.TranscribeResult) { r.Segments[1].EndMS = 12_000 },
		"start beyond end":   func(r *api.TranscribeResult) { r.Segments[1].StartMS = 10_500 },
		"huge text":          func(r *api.TranscribeResult) { r.Segments[0].Text = string(make([]byte, maxSegmentText+1)) },
		"invalid utf8":       func(r *api.TranscribeResult) { r.Segments[0].Text = "\xff" },
		"long engine string": func(r *api.TranscribeResult) { r.EngineVersion = string(make([]byte, 300)) },
		"too many segments": func(r *api.TranscribeResult) {
			r.Segments = make([]api.Segment, 10_000/minSegmentSpanMS+segmentCountSlack+1)
		},
	}
	for name, mut := range bad {
		r := ok()
		mut(&r)
		assert.Error(t, s.validateTranscribe(&r, lease), name)
	}
}

func TestParseByteSize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int64{
		"0": 0, "1048576": 1 << 20, "2GiB": 2 << 30, "2gib": 2 << 30, "1G": 1e9, "512 MiB": 512 << 20,
		"1.5k": 1500, "10B": 10, "1TiB": 1 << 40,
	} {
		got, err := ParseByteSize(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"", "-1", "2 parsecs", "GiB", "1e400"} {
		_, err := ParseByteSize(in)
		assert.Error(t, err, in)
	}
}

func TestParseFactors(t *testing.T) {
	t.Parallel()
	got, err := ParseFactors("low=1, medium=2,high=4.5")
	require.NoError(t, err)
	assert.Equal(t, map[string]float64{"low": 1, "medium": 2, "high": 4.5}, got)
	got, err = ParseFactors("")
	require.NoError(t, err)
	assert.Empty(t, got)
	for _, in := range []string{"low", "low=0", "low=-1", "=2", "low=x"} {
		_, err := ParseFactors(in)
		assert.Error(t, err, in)
	}
}

func TestParseWait(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]time.Duration{
		"": 0, "30s": 30 * time.Second, "30": 30 * time.Second, "0.5": 500 * time.Millisecond, "10m": time.Minute,
	} {
		got, err := parseWait(in, time.Minute)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"soon", "-1s", "-3"} {
		_, err := parseWait(in, time.Minute)
		assert.Error(t, err, in)
	}
}

func TestClientIP(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.1:5555"
	r.Header.Add("X-Forwarded-For", "10.0.0.1, 198.51.100.2")
	assert.Equal(t, "192.0.2.1", (&Server{}).clientIP(r), "XFF ignored without trust")
	trusting := &Server{cfg: Config{TrustProxy: true}}
	assert.Equal(t, "198.51.100.2", trusting.clientIP(r), "rightmost entry: what the proxy saw")
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	assert.Equal(t, "192.0.2.1", trusting.clientIP(r))
}

func TestIsLoopbackAddr(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "[::1]:8080": true, "localhost:80": true,
		":8080": false, "0.0.0.0:8080": false, "192.168.1.5:8080": false, "bad": false,
	} {
		assert.Equal(t, want, IsLoopbackAddr(addr), addr)
	}
}

func TestDispatcher(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	w1 := d.Wait()
	d.Notify()
	select {
	case <-w1:
	default:
		t.Fatal("waiter not woken")
	}
	w2 := d.Wait()
	select {
	case <-w2:
		t.Fatal("a new waiter must wait for the next notify")
	default:
	}
	d.Close()
	d.Close() // idempotent
	select {
	case <-d.Closing():
	default:
		t.Fatal("closing not signalled")
	}
}

func TestJobLocks(t *testing.T) {
	t.Parallel()
	l := newJobLocks()
	var inside, maxInside atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := l.Lock("job")
			n := inside.Add(1)
			if n > maxInside.Load() {
				maxInside.Store(n)
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
			unlock()
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), maxInside.Load())
	l.mu.Lock()
	defer l.mu.Unlock()
	assert.Empty(t, l.m, "entries are released")
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	c := Config{DataDir: "/x"}
	c.applyDefaults()
	require.NoError(t, c.Validate())
	c.SplitWindow = c.TaskTarget
	assert.Error(t, c.Validate())
	assert.Error(t, (&Config{}).Validate(), "data dir required")
	bs, err := blobs.New(t.TempDir())
	require.NoError(t, err)
	_, err = New(Config{DataDir: "/x", Models: []string{"silero-v5.1.2"}}, Deps{Store: &store.Store{}, Blobs: bs})
	assert.ErrorContains(t, err, "not a whisper model")
	_, err = New(Config{DataDir: "/x", VADModel: "small.en"}, Deps{Store: &store.Store{}, Blobs: bs})
	assert.ErrorContains(t, err, "not a VAD model")
	_, err = New(Config{DataDir: "/x"}, Deps{Store: &store.Store{}})
	assert.Error(t, err, "blobs required")
}
