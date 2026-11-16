package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/backoff"
	"github.com/deweysasser/whisper-network/internal/client"
	"github.com/deweysasser/whisper-network/internal/models"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/sandbox"
	"github.com/deweysasser/whisper-network/internal/testscale"
	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

// Test model files: tiny bytes, realistic memory footprints.
var (
	tinyData = bytes.Repeat([]byte("tiny-ggml "), 100)
	bigData  = bytes.Repeat([]byte("big-ggml "), 200)
	vadData  = bytes.Repeat([]byte("silero "), 50)
)

func pinned(name string, kind models.Kind, data []byte, peak int64) models.Model {
	sum := sha256.Sum256(data)
	return models.Model{Name: name, Kind: kind, File: "ggml-" + name + ".bin", SHA256: hex.EncodeToString(sum[:]),
		Size: int64(len(data)), PeakRSS: peak, CostFactor: 1}
}

func testCatalog() Catalog {
	return Catalog{
		pinned("tiny.test", models.KindWhisper, tinyData, 100*mib),
		pinned("big.test", models.KindWhisper, bigData, 3*gib),
		pinned("vad.test", models.KindVAD, vadData, 0),
	}
}

// fakeRoot is a minimal Linux /proc and cgroup v2 tree.
func fakeRoot() fstest.MapFS {
	return fstest.MapFS{
		"proc/cpuinfo":                     {Data: []byte("processor\t: 0\nmodel name\t: Fake CPU @ 3GHz\n")},
		"proc/meminfo":                     {Data: []byte("MemTotal:       16384000 kB\n")},
		"proc/loadavg":                     {Data: []byte("0.50 0.40 0.30 1/100 42\n")},
		"proc/self/cgroup":                 {Data: []byte("0::/\n")},
		"sys/fs/cgroup/cgroup.controllers": {Data: []byte("cpu memory\n")},
		"sys/fs/cgroup/cpu.stat":           {Data: []byte("usage_usec 1\nthrottled_usec 2500000\n")},
	}
}

// syncBuffer is a concurrency-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// sleepLog records every delay the worker asked for.
type sleepLog struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleepLog) add(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
}

func (s *sleepLog) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

type harness struct {
	t      *testing.T
	srv    *workertest.Server
	fakes  workertest.Fakes
	cfg    Config
	logs   *syncBuffer
	sleeps *sleepLog
}

// timeScale makes production delays run 1000× faster: a 30 s heartbeat
// interval takes 30 ms.
const timeScale = 1000

func newHarness(t *testing.T) *harness {
	t.Helper()
	srv := workertest.NewServer(t)
	for _, m := range []struct {
		name string
		data []byte
	}{{"tiny.test", tinyData}, {"big.test", bigData}, {"vad.test", vadData}} {
		srv.AddModel(m.name, m.data)
	}
	c, err := client.New(srv.URL)
	require.NoError(t, err)
	logs := &syncBuffer{}
	sleeps := &sleepLog{}
	h := &harness{t: t, srv: srv, fakes: workertest.InstallFakes(t), logs: logs, sleeps: sleeps}
	h.cfg = Config{
		Client:         c,
		StateDir:       t.TempDir(),
		WhisperCLI:     h.fakes.WhisperCLI,
		FFmpeg:         h.fakes.FFmpeg,
		Memory:         4 * gib,
		Slots:          1,
		Catalog:        testCatalog(),
		Version:        "test",
		Logger:         obs.NewLogger(zerolog.New(logs).With().Timestamp().Logger(), obs.ModeWorker),
		ClaimWait:      100 * time.Millisecond,
		PendingBackoff: backoff.Policy{Initial: 30 * time.Second, Max: 5 * time.Minute},
		RetryBackoff:   backoff.Policy{Initial: time.Second, Max: time.Minute},
		Sleep:          backoff.Scaled(timeScale, sleeps.add),
		SysRoot:        fakeRoot(),
		NumCPU:         8,
		Limits:         Limits{NoAddressSpaceLimit: true},
		FreeBytes:      func(string) (int64, error) { return 1 << 40, nil },
		ProbeSandbox: func(context.Context) (sandbox.ProbeResult, error) {
			return sandbox.ProbeResult{Level: api.SandboxFull, LandlockABI: 6}, nil
		},
	}
	return h
}

// start runs a worker built from h.cfg until the test ends or ctx is
// cancelled; the returned channel yields Run's result.
func (h *harness) start(ctx context.Context) (*Worker, <-chan error) {
	h.t.Helper()
	h.cfg.ChildEnv = append(h.cfg.ChildEnv, workertest.FastExit)
	w, err := New(h.cfg)
	require.NoError(h.t, err)
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- w.Run(ctx)
	}()
	h.t.Cleanup(func() {
		select {
		case <-finished:
		case <-time.After(testscale.D(20 * time.Second)):
			h.t.Error("worker did not stop")
		}
	})
	return w, done
}

// run starts a worker with a context cancelled at test end and returns the
// cancel function.
func (h *harness) run() (context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	_, done := h.start(ctx)
	// Registered after start's wait, so it runs first (cleanups are LIFO).
	h.t.Cleanup(cancel)
	return cancel, done
}

// worker waits for the registration and returns the server's record.
func (h *harness) worker() workertest.Worker {
	h.t.Helper()
	var w workertest.Worker
	require.Eventually(h.t, func() bool {
		var ok bool
		w, ok = h.srv.OnlyWorker()
		return ok
	}, testscale.D(10*time.Second), 5*time.Millisecond, "worker never registered")
	return w
}

// activate waits for registration and approves the worker.
func (h *harness) activate() string {
	h.t.Helper()
	w := h.worker()
	h.srv.SetState(w.ID, api.WorkerActive)
	return w.ID
}

// eventually waits up to 20 s (scaled) for cond.
func (h *harness) eventually(cond func() bool, msg string) {
	h.t.Helper()
	require.Eventually(h.t, cond, testscale.D(20*time.Second), 5*time.Millisecond, msg)
}

// waitEvent waits for the worker to log event.
func (h *harness) waitEvent(event string) {
	h.t.Helper()
	needle := []byte(`"event":"` + event + `"`)
	h.eventually(func() bool { return bytes.Contains(h.logs.Bytes(), needle) }, "the worker never logged "+event)
}

func (h *harness) taskState(id string, st api.TaskState) func() bool {
	return func() bool { return h.srv.Task(id).State == st }
}

// liveLease waits for a live lease on task and returns it.
func (h *harness) liveLease(task string) workertest.Lease {
	h.t.Helper()
	var l workertest.Lease
	h.eventually(func() bool {
		for _, c := range h.srv.LeasesFor(task) {
			if c.Live {
				l = c
				return true
			}
		}
		return false
	}, "task never leased")
	return l
}

func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(testscale.D(20 * time.Second)):
		t.Fatal("worker did not return")
		return nil
	}
}
