package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/backoff"
)

// Host load admission (DESIGN.md §7.3): a slot does not claim while the
// host's CPU is busy with work other than this worker's own.

const (
	// loadWindow is the span the host load is averaged over.
	loadWindow = time.Minute
	// loadMinGap is the least time between two stored samples; reads in
	// between reuse the samples already taken.
	loadMinGap = time.Second
	// loadRecheck is how often a slot deferred for load looks again (±20%
	// jitter, so slots do not stampede).
	loadRecheck = 30 * time.Second
	// loadWarmup is how long a slot waits, once, for a first reading when
	// the meter has only one sample.
	loadWarmup = loadMinGap
)

// hostLoadSupported reports whether the platform has /proc/stat.
const hostLoadSupported = runtime.GOOS == "linux"

// loadRecheckPolicy spaces the rechecks of a deferred slot.
var loadRecheckPolicy = backoff.Policy{Initial: loadRecheck, Max: loadRecheck, Jitter: 0.2}

// CPUTimes are cumulative CPU times in clock ticks (USER_HZ).
type CPUTimes struct {
	// Total is all CPU time of the host, idle included.
	Total uint64
	// Busy is the non-idle part of Total: everything but idle and iowait.
	Busy uint64
	// Own is the CPU time of the worker's process tree: the worker, its
	// live descendants, and the descendants they have reaped.
	Own uint64
}

// CheckHostLoad validates the load thresholds (percent): maxLoad in
// [0, 100], 0 disabling the check, and resume in [0, maxLoad].
func CheckHostLoad(maxLoad, resume float64) error {
	switch {
	case maxLoad < 0 || maxLoad > 100 || math.IsNaN(maxLoad):
		return fmt.Errorf("--max-host-load must be a percentage from 0 (disabled) to 100, not %g", maxLoad)
	case maxLoad == 0:
		return nil
	case resume < 0 || math.IsNaN(resume):
		return fmt.Errorf("--max-host-load-resume must be a percentage, not %g", resume)
	case resume > maxLoad:
		return fmt.Errorf("--max-host-load-resume (%g) must not exceed --max-host-load (%g)", resume, maxLoad)
	}
	return nil
}

// loadSample is one reading of the cumulative CPU times.
type loadSample struct {
	at  time.Time
	cpu CPUTimes
}

// loadMeter turns cumulative CPU times into the host's CPU utilisation over
// the last loadWindow, excluding the worker's own process tree. Samples are
// taken when read, at most one per loadMinGap; the counters are cumulative,
// so the average is exact however unevenly the samples fall.
type loadMeter struct {
	read func() (CPUTimes, error)
	now  func() time.Time

	mu      sync.Mutex
	samples []loadSample
	failed  bool
}

func newLoadMeter(read func() (CPUTimes, error)) *loadMeter {
	return &loadMeter{read: read, now: time.Now}
}

// percent returns the utilisation in percent, false while unknown (fewer
// than two samples, or no time passed between them).
func (m *loadMeter) percent() (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if n := len(m.samples); n < 2 || now.Sub(m.samples[n-1].at) >= loadMinGap {
		if cpu, err := m.read(); err == nil {
			m.samples = append(m.samples, loadSample{at: now, cpu: cpu})
		} else {
			m.failed = true
		}
	}
	// Keep the newest sample at least loadWindow old as the base.
	for len(m.samples) >= 2 && !m.samples[1].at.After(now.Add(-loadWindow)) {
		m.samples = m.samples[1:]
	}
	if len(m.samples) < 2 {
		return 0, false
	}
	return utilisation(m.samples[0].cpu, m.samples[len(m.samples)-1].cpu)
}

// utilisation is the busy share, in percent, of the host's CPU time between
// two readings, less the worker's own. Counters that went backwards (a
// process left the tree without being reaped inside it) count as zero.
func utilisation(a, b CPUTimes) (float64, bool) {
	if b.Total <= a.Total {
		return 0, false
	}
	total := float64(b.Total - a.Total)
	busy := float64(sub(b.Busy, a.Busy))
	own := float64(sub(b.Own, a.Own))
	pct := 100 * (busy - own) / total
	return math.Round(min(max(pct, 0), 100)*10) / 10, true
}

func sub(b, a uint64) uint64 {
	if b < a {
		return 0
	}
	return b - a
}

// loadGate holds slots back from claiming while the host is busy.
type loadGate struct {
	maxLoad, resume float64
	load            func() (float64, bool)

	deferring atomic.Int32 // slots currently deferred
	deferrals atomic.Int64 // claims deferred since start
}

// slotLoad is one slot's hysteresis state.
type slotLoad struct {
	deferred bool
	// warmed is set once the slot has waited for a first reading.
	warmed bool
}

// enabled reports whether the gate checks anything.
func (g *loadGate) enabled() bool { return g != nil && g.maxLoad > 0 && g.load != nil }

// admit decides whether slot s may claim now, updating its hysteresis
// state: a slot that may claim stops when the load exceeds maxLoad, and a
// deferred slot resumes when it falls below resume. An unknown load admits.
func (g *loadGate) admit(s *slotLoad) (load float64, known, ok bool) {
	if !g.enabled() {
		return 0, false, true
	}
	load, known = g.load()
	was := s.deferred
	switch {
	case !known:
		s.deferred = false
	case s.deferred:
		s.deferred = load >= g.resume
	default:
		s.deferred = load > g.maxLoad
	}
	if s.deferred != was {
		if s.deferred {
			g.deferring.Add(1)
		} else {
			g.deferring.Add(-1)
		}
	}
	if s.deferred {
		g.deferrals.Add(1)
	}
	return load, known, !s.deferred
}

// leave clears a slot's deferral when it stops.
func (g *loadGate) leave(s *slotLoad) {
	if g != nil && s.deferred {
		s.deferred = false
		g.deferring.Add(-1)
	}
}

// waitForLoad blocks a slot until the host load lets it claim. It returns
// false when ctx ends. While deferred the slot reports the worker's gauges,
// since it sends no claims: the server keeps seeing the worker, and shows
// why it takes no work. Running tasks are not affected.
func (w *Worker) waitForLoad(ctx context.Context, s *slotLoad, log zerolog.Logger) bool {
	for {
		was := s.deferred
		load, known, ok := w.load.admit(s)
		if ok {
			if !known && !s.warmed {
				// One sample so far (startup): wait for a reading
				// rather than claim blind on a busy host.
				s.warmed = true
				if w.cfg.Sleep(ctx, loadWarmup) != nil {
					return false
				}
				continue
			}
			if was {
				log.Info().Float64("host_load", load).Float64("resume_below", w.load.resume).
					Msg("host load fell; slot resumes claiming")
			}
			return ctx.Err() == nil
		}
		delay := loadRecheckPolicy.Delay(0)
		ev := log.Debug()
		if !was {
			ev = log.Info()
		}
		ev.Float64("host_load", load).Float64("max_host_load", w.load.maxLoad).
			Float64("resume_below", w.load.resume).Dur("retry_in", delay).Msg("host busy; slot not claiming")
		w.reportGauges(ctx)
		if w.cfg.Sleep(ctx, delay) != nil {
			return false
		}
	}
}

// reportGauges sends the gauges without claiming.
func (w *Worker) reportGauges(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	g := w.gauges()
	if err := w.api.ReportGauges(ctx, w.creds.ID, &g); err != nil {
		if isFatal(err) {
			w.fail(err)
			return
		}
		w.log.Debug().Err(err).Msg("could not report gauges")
	}
}

// ---------------------------------------------------------------------------
// Reading /proc

// errNoProcStat means the platform has no /proc/stat.
var errNoProcStat = errors.New("no /proc/stat")

// CPUTimes reads the host's cumulative CPU times from proc/stat and the
// worker's own process tree from proc/<pid>/stat.
//
// The tree's time is the sum, over the worker and its live descendants, of
// utime + stime + cutime + cstime: a live process's own time, plus the time
// of every descendant it has waited for. Each process's time is counted
// once, while it runs in its own fields and once reaped in its parent's.
func (s System) CPUTimes() (CPUTimes, error) {
	b, err := fs.ReadFile(s.Root, "proc/stat")
	if err != nil {
		return CPUTimes{}, errNoProcStat
	}
	var t CPUTimes
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return CPUTimes{}, fmt.Errorf("proc/stat: unexpected first line %q", line)
	}
	// user nice system idle iowait irq softirq steal [guest guest_nice]:
	// guest time is already included in user and nice.
	for i, v := range f[1:min(len(f), 9)] {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return CPUTimes{}, fmt.Errorf("proc/stat: %w", err)
		}
		t.Total += n
		if i != 3 && i != 4 { // idle, iowait
			t.Busy += n
		}
	}
	t.Own = s.treeTicks()
	return t, nil
}

// procStat is the part of /proc/<pid>/stat the tree needs.
type procStat struct {
	pid, ppid int
	ticks     uint64 // utime + stime + cutime + cstime
}

// parseProcStat parses a /proc/<pid>/stat line. The command name is in
// parentheses and may contain spaces and parentheses itself, so the fields
// are taken after the last ')'.
func parseProcStat(b []byte) (procStat, bool) {
	s := string(b)
	open, closing := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 1 || closing < open {
		return procStat{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(s[:open]))
	if err != nil {
		return procStat{}, false
	}
	// state ppid pgrp session tty tpgid flags minflt cminflt majflt
	// cmajflt utime stime cutime cstime …
	f := strings.Fields(s[closing+1:])
	if len(f) < 15 {
		return procStat{}, false
	}
	p := procStat{pid: pid}
	if p.ppid, err = strconv.Atoi(f[1]); err != nil {
		return procStat{}, false
	}
	for _, v := range f[11:15] {
		n, err := strconv.ParseInt(v, 10, 64) // cutime/cstime are signed
		if err != nil {
			return procStat{}, false
		}
		p.ticks += uint64(max(n, 0))
	}
	return p, true
}

// treeTicks is the CPU time of this process and all its descendants.
func (s System) treeTicks() uint64 {
	b, err := fs.ReadFile(s.Root, "proc/self/stat")
	if err != nil {
		return 0
	}
	self, ok := parseProcStat(b)
	if !ok {
		return 0
	}
	ents, err := fs.ReadDir(s.Root, "proc")
	if err != nil {
		return self.ticks
	}
	kids := map[int][]procStat{}
	for _, e := range ents {
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		b, err := fs.ReadFile(s.Root, "proc/"+e.Name()+"/stat")
		if err != nil {
			continue // exited
		}
		if p, ok := parseProcStat(b); ok && p.pid != self.pid {
			kids[p.ppid] = append(kids[p.ppid], p)
		}
	}
	total := self.ticks
	queue := []int{self.pid}
	seen := map[int]bool{self.pid: true}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, k := range kids[pid] {
			if !seen[k.pid] {
				seen[k.pid] = true
				total += k.ticks
				queue = append(queue, k.pid)
			}
		}
	}
	return total
}

// newLoadGate builds the gate from the config. The load is measured on
// Linux whether or not the gate is enabled, for the host_load gauge; other
// platforms (macOS and Windows are not production worker platforms) do not
// measure it, and an enabled gate is disabled with a warning.
func (w *Worker) newLoadGate() *loadGate {
	g := &loadGate{maxLoad: w.cfg.MaxHostLoad, resume: w.cfg.MaxHostLoadResume, load: w.cfg.HostLoad}
	if g.load != nil {
		return g
	}
	if !hostLoadSupported {
		if g.maxLoad > 0 {
			w.log.Warn().Str("os", runtime.GOOS).Msg("host CPU load is not measured on this platform; --max-host-load is ignored")
		}
		return g
	}
	if _, err := w.sys.CPUTimes(); err != nil {
		if g.maxLoad > 0 {
			w.log.Warn().Err(err).Msg("cannot read host CPU load; --max-host-load is ignored")
		}
		return g
	}
	m := newLoadMeter(w.sys.CPUTimes)
	m.percent() // the first sample
	g.load = m.percent
	return g
}

// gauges adds the load readings to gg.
func (g *loadGate) gauges(gg *api.Gauges) {
	if g == nil || g.load == nil {
		return
	}
	if pct, ok := g.load(); ok {
		gg.HostLoad = ptr(pct)
	}
	if g.enabled() {
		gg.DeferredForLoad = g.deferring.Load() > 0
		gg.ClaimsDeferred = ptr(g.deferrals.Load())
	}
}
