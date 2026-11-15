package worker

import (
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCgroupV2NestedLimits(t *testing.T) {
	root := fstest.MapFS{
		"proc/self/cgroup":                                 {Data: []byte("0::/system.slice/wn.service\n")},
		"proc/meminfo":                                     {Data: []byte("MemTotal:       16000000 kB\n")},
		"sys/fs/cgroup/cgroup.controllers":                 {Data: []byte("cpu memory\n")},
		"sys/fs/cgroup/system.slice/cpu.max":               {Data: []byte("150000 100000\n")},
		"sys/fs/cgroup/system.slice/memory.max":            {Data: []byte("max\n")},
		"sys/fs/cgroup/system.slice/wn.service/cpu.max":    {Data: []byte("max 100000\n")},
		"sys/fs/cgroup/system.slice/wn.service/memory.max": {Data: []byte("6442450944\n")},
		"sys/fs/cgroup/system.slice/wn.service/cpu.stat":   {Data: []byte("usage_usec 10\nthrottled_usec 1500000\n")},
	}
	s := System{Root: root, NumCPU: 8}
	assert.Equal(t, CgroupLimits{CPU: 1.5, Memory: 6 << 30}, s.Cgroup(), "the tightest limit on the path wins")
	assert.Equal(t, 1, s.UsableCores(), "1.5 cores round down")
	assert.Equal(t, int64(6<<30), s.MemoryLimit(), "cgroup limit below physical memory")
	assert.Equal(t, 1500*time.Millisecond, s.CPUThrottled())
}

func TestCgroupV2InContainerNamespace(t *testing.T) {
	root := fstest.MapFS{
		"proc/self/cgroup":                 {Data: []byte("0::/\n")},
		"proc/meminfo":                     {Data: []byte("MemTotal:       2000000 kB\n")},
		"sys/fs/cgroup/cgroup.controllers": {Data: []byte("cpu memory\n")},
		"sys/fs/cgroup/cpu.max":            {Data: []byte("400000 100000\n")},
		"sys/fs/cgroup/memory.max":         {Data: []byte("8589934592\n")},
	}
	s := System{Root: root, NumCPU: 16}
	assert.Equal(t, 4, s.UsableCores())
	assert.Equal(t, int64(2000000*1024), s.MemoryLimit(), "physical memory below the cgroup limit")
}

func TestCgroupV1(t *testing.T) {
	root := fstest.MapFS{
		"proc/self/cgroup": {Data: []byte("12:memory:/docker/abc\n4:cpu,cpuacct:/docker/abc\n1:name=systemd:/docker/abc\n")},
		// Without a cgroup namespace the container sees the hierarchy root.
		"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_quota_us":            {Data: []byte("250000\n")},
		"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_period_us":           {Data: []byte("100000\n")},
		"sys/fs/cgroup/cpu,cpuacct/cpu.stat":                    {Data: []byte("nr_periods 5\nthrottled_time 2000000000\n")},
		"sys/fs/cgroup/memory/docker/abc/memory.limit_in_bytes": {Data: []byte("1073741824\n")},
	}
	s := System{Root: root, NumCPU: 8}
	assert.Equal(t, CgroupLimits{CPU: 2.5, Memory: 1 << 30}, s.Cgroup())
	assert.Equal(t, 2, s.UsableCores())
	assert.Equal(t, 2*time.Second, s.CPUThrottled())
}

func TestCgroupV1Unlimited(t *testing.T) {
	root := fstest.MapFS{
		"proc/self/cgroup":                            {Data: []byte("4:memory:/\n3:cpu,cpuacct:/\n")},
		"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_quota_us":  {Data: []byte("-1\n")},
		"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_period_us": {Data: []byte("100000\n")},
		"sys/fs/cgroup/memory/memory.limit_in_bytes":  {Data: []byte("9223372036854771712\n")},
	}
	assert.Equal(t, CgroupLimits{}, System{Root: root}.Cgroup())
}

func TestNoProcfs(t *testing.T) {
	s := System{Root: fstest.MapFS{}, NumCPU: 3}
	assert.Equal(t, 3, s.UsableCores())
	assert.Equal(t, 0.0, s.Load1())
	assert.Equal(t, int64(0), s.RSS(1))
	assert.Equal(t, time.Duration(0), s.CPUThrottled())
}

func TestProcReadings(t *testing.T) {
	root := fstest.MapFS{
		"proc/cpuinfo":   {Data: []byte("processor\t: 0\nmodel name\t: Example  CPU @ 3.00GHz\n\nprocessor\t: 1\nmodel name\t: Example  CPU @ 3.00GHz\n")},
		"proc/loadavg":   {Data: []byte("1.25 0.5 0.1 2/300 99\n")},
		"proc/42/status": {Data: []byte("Name:\twhisper-cli\nVmRSS:\t  204800 kB\n")},
		"proc/meminfo":   {Data: []byte("MemTotal:  1024 kB\n")},
	}
	s := System{Root: root, NumCPU: 2}
	assert.Equal(t, "Example CPU @ 3.00GHz", s.CPUModel())
	assert.Equal(t, 1.25, s.Load1())
	assert.Equal(t, int64(200<<20), s.RSS(42))
	assert.Equal(t, int64(1<<20), s.MemoryLimit())

	arm := System{Root: fstest.MapFS{"proc/cpuinfo": {Data: []byte("processor\t: 0\nHardware\t: BCM2835\n")}}}
	assert.Equal(t, "BCM2835", arm.CPUModel())
}

func TestSlotCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   SlotInput
		want int
	}{
		{"cpu bound", SlotInput{UsableCores: 8, ThreadsPerSlot: 4, Budget: 15 * gib, LargestPeak: 1200 * mib}, 2},
		{"memory bound", SlotInput{UsableCores: 32, ThreadsPerSlot: 4, Budget: 5 * gib, LargestPeak: 2800 * mib}, 1},
		{"fewer cores than threads", SlotInput{UsableCores: 2, ThreadsPerSlot: 4, Budget: 8 * gib, LargestPeak: gib}, 1},
		{"prepare only", SlotInput{UsableCores: 8, ThreadsPerSlot: 4, Budget: gib, PrepareMemory: 512 * mib}, 2},
		{"nothing fits", SlotInput{UsableCores: 8, ThreadsPerSlot: 4, Budget: 100 * mib, LargestPeak: gib}, 0},
		{"no budget", SlotInput{UsableCores: 8, ThreadsPerSlot: 4, Budget: -1, LargestPeak: gib}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, SlotCount(tc.in)) })
	}
}

func TestModelsThatFit(t *testing.T) {
	names, largest := modelsThatFit(testCatalog(), 2*gib)
	assert.Equal(t, []string{"tiny.test"}, names, "VAD models are never offered as whisper models")
	assert.Equal(t, 100*mib, largest)
	names, largest = modelsThatFit(testCatalog(), 3*gib)
	assert.Equal(t, []string{"tiny.test", "big.test"}, names)
	assert.Equal(t, 3*gib, largest)
}
