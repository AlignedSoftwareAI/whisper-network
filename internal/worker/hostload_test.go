package worker

import (
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

// procStatLine is a /proc/<pid>/stat line with the given CPU ticks.
func procStatLine(pid, ppid int, comm string, utime, stime, cutime, cstime int) []byte {
	return fmt.Appendf(nil, "%d (%s) S %d %d %d 0 -1 4194560 100 0 0 0 %d %d %d %d 20 0 1 0 100 1000 10\n",
		pid, comm, ppid, pid, pid, utime, stime, cutime, cstime)
}

func TestParseProcStat(t *testing.T) {
	p, ok := parseProcStat(procStatLine(42, 7, "evil) S 1 (x", 10, 5, 3, 2))
	require.True(t, ok, "the command name may contain parentheses and spaces")
	assert.Equal(t, procStat{pid: 42, ppid: 7, ticks: 20}, p)
	_, ok = parseProcStat([]byte("garbage"))
	assert.False(t, ok)
}

// The worker's own tree is itself, every live descendant (however deep) and
// what they reaped; unrelated processes are not.
func TestCPUTimesOwnTree(t *testing.T) {
	root := fstest.MapFS{
		"proc/stat": {Data: []byte("cpu  1000 200 300 5000 100 10 20 30 400 0\ncpu0 1 2 3 4 5 6 7 8 0 0\n")},
		// The worker (100) has reaped children worth 7 ticks.
		"proc/self/stat": {Data: procStatLine(100, 1, "whisper-network", 10, 5, 4, 3)},
		"proc/100/stat":  {Data: procStatLine(100, 1, "whisper-network", 10, 5, 4, 3)},
		"proc/200/stat":  {Data: procStatLine(200, 100, "sandbox-exec", 1, 1, 0, 0)},
		"proc/300/stat":  {Data: procStatLine(300, 200, "whisper cli", 500, 50, 0, 0)},
		"proc/400/stat":  {Data: procStatLine(400, 1, "other", 900, 90, 0, 0)},
		"proc/500/stat":  {Data: procStatLine(500, 400, "other-child", 9, 9, 0, 0)},
	}
	got, err := System{Root: root}.CPUTimes()
	require.NoError(t, err)
	assert.Equal(t, CPUTimes{
		Total: 1000 + 200 + 300 + 5000 + 100 + 10 + 20 + 30, // guest is already in user
		Busy:  1000 + 200 + 300 + 10 + 20 + 30,              // not idle or iowait
		Own:   22 + 2 + 550,
	}, got)

	_, err = System{Root: fstest.MapFS{}}.CPUTimes()
	assert.ErrorIs(t, err, errNoProcStat)
}

// fakeCPU is a cumulative CPU counter advanced by the test.
type fakeCPU struct {
	t   CPUTimes
	now time.Time
}

// run advances by d on a host with cores CPUs, of which otherCores are busy
// with other work and ownCores with the worker's tree.
func (f *fakeCPU) run(d time.Duration, cores, otherCores, ownCores float64) {
	ticks := func(c float64) uint64 { return uint64(math.Round(c * d.Seconds() * 100)) }
	f.t.Total += ticks(cores)
	f.t.Busy += ticks(otherCores + ownCores)
	f.t.Own += ticks(ownCores)
	f.now = f.now.Add(d)
}

func newFakeMeter() (*fakeCPU, *loadMeter) {
	f := &fakeCPU{now: time.Unix(1000, 0)}
	m := newLoadMeter(func() (CPUTimes, error) { return f.t, nil })
	m.now = func() time.Time { return f.now }
	return f, m
}

func TestLoadMeterExcludesOwnTree(t *testing.T) {
	f, m := newFakeMeter()
	_, ok := m.percent()
	assert.False(t, ok, "one sample is not a reading")

	// 8 cores: the worker's task uses 4 (50%), other work 1 (12.5%).
	f.run(10*time.Second, 8, 1, 4)
	pct, ok := m.percent()
	require.True(t, ok)
	assert.InDelta(t, 12.5, pct, 0.1)

	// A worker running one 4-thread task on an 8-core box must still take a
	// second one at the default threshold.
	g := &loadGate{maxLoad: 70, resume: 60, load: m.percent}
	var s slotLoad
	_, _, admitted := g.admit(&s)
	assert.True(t, admitted, "the worker's own load does not defer claims")

	// Other work at 6 of 8 cores (75%) does.
	f.run(2*time.Minute, 8, 6, 2)
	pct, _ = m.percent()
	assert.InDelta(t, 75, pct, 0.1)
	_, _, admitted = g.admit(&s)
	assert.False(t, admitted)
}

// The reading averages the last minute: an old burst ages out.
func TestLoadMeterWindow(t *testing.T) {
	f, m := newFakeMeter()
	m.percent()
	for range 30 { // a busy minute, read every 2 s
		f.run(2*time.Second, 4, 4, 0)
		m.percent()
	}
	pct, _ := m.percent()
	assert.InDelta(t, 100, pct, 0.1)
	for range 15 { // half a minute idle
		f.run(2*time.Second, 4, 0, 0)
		m.percent()
	}
	pct, _ = m.percent()
	assert.InDelta(t, 50, pct, 2)
	for range 31 {
		f.run(2*time.Second, 4, 0, 0)
		m.percent()
	}
	pct, _ = m.percent()
	assert.InDelta(t, 0, pct, 0.1)
	assert.LessOrEqual(t, len(m.samples), 32, "samples older than the window are dropped")

	// Reads closer together than loadMinGap reuse the samples.
	n := len(m.samples)
	f.now = f.now.Add(loadMinGap / 2)
	m.percent()
	assert.Len(t, m.samples, n)
}

// A process leaving the tree unreaped makes the own counter go backwards;
// the reading stays within 0–100.
func TestUtilisationClamps(t *testing.T) {
	a := CPUTimes{Total: 1000, Busy: 500, Own: 400}
	pct, ok := utilisation(a, CPUTimes{Total: 2000, Busy: 1500, Own: 100})
	require.True(t, ok)
	assert.InDelta(t, 100, pct, 0.01)
	pct, _ = utilisation(a, CPUTimes{Total: 2000, Busy: 600, Own: 900})
	assert.InDelta(t, 0, pct, 0.01)
	_, ok = utilisation(a, a)
	assert.False(t, ok, "no time passed")
}

func TestLoadGateHysteresis(t *testing.T) {
	var load atomic.Value
	known := true
	g := &loadGate{maxLoad: 70, resume: 60, load: func() (float64, bool) { return load.Load().(float64), known }}
	var a, b slotLoad
	for i, step := range []struct {
		load     float64
		admitted bool
	}{
		{50, true},
		{70, true}, // at the threshold: still claims
		{75, false},
		{65, false}, // between resume and max: stays deferred
		{60, false},
		{59, true},
		{65, true}, // between: stays claiming
		{71, false},
	} {
		load.Store(step.load)
		_, _, ok := g.admit(&a)
		assert.Equal(t, step.admitted, ok, "step %d (load %g)", i, step.load)
		assert.Equal(t, !step.admitted, g.deferring.Load() == 1, "step %d", i)
	}
	assert.Equal(t, int64(4), g.deferrals.Load())

	// Slots are independent; leaving clears a slot's deferral.
	_, _, ok := g.admit(&b)
	assert.False(t, ok)
	assert.Equal(t, int32(2), g.deferring.Load())
	g.leave(&a)
	g.leave(&b)
	assert.Zero(t, g.deferring.Load())

	// An unknown load admits.
	known = false
	_, _, ok = g.admit(&a)
	assert.True(t, ok)

	// 0 disables the check whatever the load.
	known = true
	load.Store(100.0)
	off := &loadGate{maxLoad: 0, resume: 0, load: g.load}
	assert.False(t, off.enabled())
	_, _, ok = off.admit(&a)
	assert.True(t, ok)
}

func TestCheckHostLoad(t *testing.T) {
	for _, c := range []struct {
		max, resume float64
		ok          bool
	}{
		{70, 60, true},
		{70, 70, true},
		{0, 0, true},
		{0, 90, true}, // disabled: resume is irrelevant
		{100, 0, true},
		{70, 80, false},
		{-1, 0, false},
		{101, 60, false},
		{70, -5, false},
		{math.NaN(), 0, false},
	} {
		err := CheckHostLoad(c.max, c.resume)
		assert.Equal(t, c.ok, err == nil, "max %g resume %g: %v", c.max, c.resume, err)
	}
	h := newHarness(t)
	h.cfg.MaxHostLoad, h.cfg.MaxHostLoadResume = 50, 60
	_, err := New(h.cfg)
	assert.ErrorContains(t, err, "must not exceed")
}

// hostLoad is an injectable load reading.
type hostLoad struct{ v atomic.Int64 } // tenths of a percent

func (l *hostLoad) set(pct float64) { l.v.Store(int64(pct * 10)) }
func (l *hostLoad) read() (float64, bool) {
	return float64(l.v.Load()) / 10, true
}

// A busy host stops claims and is reported as such; when it quiets down
// the worker claims again. A task already running is not affected.
func TestBusyHostDefersClaims(t *testing.T) {
	h := newHarness(t)
	load := &hostLoad{}
	load.set(90)
	h.cfg.MaxHostLoad, h.cfg.MaxHostLoadResume, h.cfg.HostLoad = 70, 60, load.read
	h.cfg.ChildEnv = []string{workertest.WhisperMode(workertest.SleepMode("300ms"))}
	task := h.srv.AddTask(workertest.Task{Kind: api.KindTranscribe, Input: workertest.WAV(2), Model: "tiny.test"})
	_, _ = h.run()
	h.activate()

	h.eventually(func() bool { return len(h.srv.GaugeReports()) >= 2 }, "a deferred worker never reported its gauges")
	assert.Empty(t, h.srv.Claims(), "no claims while the host is busy")
	g := h.srv.GaugeReports()[0]
	assert.True(t, g.DeferredForLoad)
	assert.Equal(t, ptr(90.0), g.HostLoad)
	require.NotNil(t, g.ClaimsDeferred)
	assert.Positive(t, *g.ClaimsDeferred)
	for _, d := range h.sleeps.all() {
		if d > 2*time.Second { // the warmup and the rechecks
			assert.InDelta(t, loadRecheck, d, float64(loadRecheck)/5+1, "rechecks are jittered around 30 s")
		}
	}

	// Below max but above resume: still deferred (hysteresis).
	load.set(65)
	n := len(h.srv.GaugeReports())
	h.eventually(func() bool { return len(h.srv.GaugeReports()) > n+1 }, "no further reports")
	assert.Empty(t, h.srv.Claims())

	// Quiet: the slot claims, and the claim says it is no longer deferred.
	load.set(10)
	h.liveLease(task)
	c := h.srv.Claims()[0].Gauges
	require.NotNil(t, c)
	assert.False(t, c.DeferredForLoad)
	assert.Equal(t, ptr(10.0), c.HostLoad)

	// The host gets busy while the task runs: it finishes anyway.
	load.set(99)
	h.eventually(h.taskState(task, api.TaskDone), "the running task did not finish")
	assert.Len(t, h.srv.LeasesFor(task), 1)
	assert.Equal(t, api.OutcomeDone, h.srv.LeasesFor(task)[0].Outcome)
	n = len(h.srv.Claims())
	h.eventually(func() bool {
		rs := h.srv.GaugeReports()
		return len(rs) > 0 && rs[len(rs)-1].HostLoad != nil && *rs[len(rs)-1].HostLoad == 99
	}, "the slot did not defer again")
	assert.Len(t, h.srv.Claims(), n, "no new claims while busy")
}

// --max-host-load 0 claims whatever the load, and reports no deferral.
func TestHostLoadCheckDisabled(t *testing.T) {
	h := newHarness(t)
	load := &hostLoad{}
	load.set(100)
	h.cfg.HostLoad = load.read
	_, _ = h.run()
	h.activate()
	h.eventually(func() bool { return len(h.srv.Claims()) > 0 }, "never claimed")
	g := h.srv.Claims()[0].Gauges
	require.NotNil(t, g)
	assert.Equal(t, ptr(100.0), g.HostLoad, "the load is still reported")
	assert.False(t, g.DeferredForLoad)
	assert.Nil(t, g.ClaimsDeferred)
	assert.Empty(t, h.srv.GaugeReports())
}

// Without /proc/stat an enabled check is ignored with a warning.
func TestHostLoadUnreadableIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxHostLoad, h.cfg.MaxHostLoadResume = 70, 60
	_, _ = h.run()
	h.activate()
	h.eventually(func() bool { return len(h.srv.Claims()) > 0 }, "never claimed")
	assert.Contains(t, string(h.logs.Bytes()), "--max-host-load is ignored")
	assert.Nil(t, h.srv.Claims()[0].Gauges.HostLoad)
}
