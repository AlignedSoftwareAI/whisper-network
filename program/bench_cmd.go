package program

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"text/tabwriter"
	"time"

	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/models"
	"github.com/deweysasser/whisper-network/internal/worker"
)

// BenchCmd measures whisper-cli's speed and memory on this machine for
// several thread counts (DESIGN.md §7.3), to tune --threads-per-slot and the
// model peak-RSS table.
type BenchCmd struct {
	Model      string        `default:"small.en" help:"Model to benchmark"`
	Threads    []int         `default:"2,4,8" sep:"," help:"Thread counts to try"`
	Audio      string        `type:"existingfile" help:"Canonical 16 kHz mono 16-bit WAV to transcribe (default: 60 s of synthetic audio)"`
	Language   string        `default:"en" help:"Language passed to whisper-cli"`
	WhisperCLI string        `name:"whisper-cli" default:"whisper-cli" env:"WHISPER_WHISPER_CLI" help:"whisper-cli binary"`
	Cache      string        `type:"path" env:"WHISPER_BENCH_CACHE" help:"Model cache directory (default: the user cache dir)"`
	Timeout    time.Duration `default:"30m" help:"Time limit per run"`

	// Test seams.
	out     io.Writer
	catalog worker.Catalog
	fetch   worker.Fetcher
}

// benchRun is one measured whisper-cli run.
type benchRun struct {
	threads int
	wall    time.Duration
	maxRSS  int64
}

// Run benchmarks each thread count and suggests --threads-per-slot.
func (c *BenchCmd) Run(ctx context.Context) error {
	out := stdout(c.out)
	whisper, err := exec.LookPath(c.WhisperCLI)
	if err != nil {
		return fmt.Errorf("whisper-cli not found (%w); install whisper.cpp or pass --whisper-cli", err)
	}
	catalog := c.catalog
	if catalog == nil {
		catalog = models.All()
	}
	m, ok := catalog.Lookup(c.Model)
	if !ok || m.Kind != models.KindWhisper {
		return fmt.Errorf("unknown model %q", c.Model)
	}
	cacheDir := c.Cache
	if cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("no cache directory; pass --cache: %w", err)
		}
		cacheDir = filepath.Join(base, "whisper-network", "models")
	}
	fetch := c.fetch
	if fetch == nil {
		fetch = fetchUpstream
	}
	fmt.Fprintf(out, "model %s: ensuring a verified copy in %s\n", m.Name, cacheDir)
	modelPath, err := worker.NewModelCache(ctx, cacheDir, fetch).Path(ctx, m)
	if err != nil {
		return err
	}

	audio, seconds, synthetic, cleanup, err := c.audio()
	if err != nil {
		return err
	}
	defer cleanup()
	if synthetic {
		fmt.Fprintln(out, "audio: 60 s of synthetic tones and noise; this measures speed and memory, not accuracy")
	}

	var runs []benchRun
	for _, n := range c.Threads {
		if n <= 0 {
			return fmt.Errorf("thread count %d must be positive", n)
		}
		r, err := c.runOnce(ctx, whisper, modelPath, audio, n)
		if err != nil {
			return err
		}
		runs = append(runs, r)
		fmt.Fprintf(out, "  -t %d: %s\n", n, r.wall.Round(time.Millisecond))
	}
	cores := worker.System{Root: os.DirFS("/"), NumCPU: runtime.NumCPU()}.UsableCores()
	return report(out, runs, seconds, cores)
}

func (c *BenchCmd) runOnce(ctx context.Context, whisper, model, audio string, threads int) (benchRun, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, whisper, "-m", model, "-f", audio, "-l", c.Language,
		"-t", fmt.Sprint(threads), "-np")
	var stderr tail
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		return benchRun{}, fmt.Errorf("whisper-cli -t %d: %w\n%s", threads, err, stderr.String())
	}
	return benchRun{threads: threads, wall: time.Since(start), maxRSS: maxRSS(cmd.ProcessState)}, nil
}

// audio returns the WAV to transcribe and its duration in seconds.
func (c *BenchCmd) audio() (path string, seconds float64, synthetic bool, cleanup func(), err error) {
	cleanup = func() {}
	if c.Audio != "" {
		f, err := os.Open(c.Audio)
		if err != nil {
			return "", 0, false, cleanup, err
		}
		defer f.Close()
		w, err := media.OpenWAV(f, 0)
		if err != nil {
			return "", 0, false, cleanup, fmt.Errorf("--audio must be a 16 kHz mono 16-bit WAV: %w", err)
		}
		return c.Audio, w.Duration().Seconds(), false, cleanup, nil
	}
	dir, err := os.MkdirTemp("", "wn-bench-")
	if err != nil {
		return "", 0, false, cleanup, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "synthetic.wav")
	if err := os.WriteFile(path, syntheticWAV(60), 0o600); err != nil {
		cleanup()
		return "", 0, false, func() {}, err
	}
	return path, 60, true, cleanup, nil
}

// syntheticWAV is deterministic speech-band audio: gliding tones in short
// bursts over low noise, with pauses, so the decoder does real work.
func syntheticWAV(seconds int) []byte {
	n := int64(seconds) * media.SampleRate
	h := media.WAVHeader(n)
	out := append(make([]byte, 0, len(h)+int(n)*2), h[:]...)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range n {
		t := float64(i) / media.SampleRate
		v := 300 * rng.NormFloat64()
		if math.Mod(t, 2.5) < 1.8 { // burst, then a pause
			f := 180 + 120*math.Sin(2*math.Pi*0.7*t)
			v += 6000*math.Sin(2*math.Pi*f*t) + 2500*math.Sin(2*math.Pi*2.7*f*t)
		}
		out = binary.LittleEndian.AppendUint16(out, uint16(int16(max(-32768, min(32767, v)))))
	}
	return out
}

// report prints the table and the suggestion. Machine throughput with t
// threads per slot is slots × audio/wall where slots = cores/t; the
// suggestion maximizes it, preferring more threads (lower per-job latency)
// within 5 %.
func report(out io.Writer, runs []benchRun, audioS float64, cores int) error {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "threads\twall\tRTF\tx realtime\tpeak RSS\tslots\tmachine x realtime\t")
	if len(runs) == 0 {
		return errors.New("no runs")
	}
	tps := make([]float64, len(runs))
	for i, r := range runs {
		rtf := r.wall.Seconds() / audioS
		slots := max(1, cores/r.threads)
		tp := float64(slots) / rtf
		tps[i] = tp
		rss := "n/a"
		if r.maxRSS > 0 {
			rss = fmt.Sprintf("%d MiB", r.maxRSS>>20)
		}
		fmt.Fprintf(tw, "%d\t%s\t%.3f\t%.1f\t%s\t%d\t%.1f\t\n", r.threads, r.wall.Round(time.Millisecond),
			rtf, 1/rtf, rss, slots, tp)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	top := 0.0
	for _, tp := range tps {
		top = max(top, tp)
	}
	best := 0
	for i, tp := range tps {
		if tp >= 0.95*top && (tps[best] < 0.95*top || runs[i].threads > runs[best].threads) {
			best = i
		}
	}
	_, err := fmt.Fprintf(out, "\nusable cores: %d\nsuggested: --threads-per-slot %d\n", cores, runs[best].threads)
	return err
}

// fetchUpstream downloads a model from its upstream URL.
func fetchUpstream(ctx context.Context, m models.Model) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", m.URL, resp.Status)
	}
	return resp.Body, nil
}

// tail keeps the last 4 KiB written to it.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string { return string(t.b) }
