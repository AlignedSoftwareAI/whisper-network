package worker

import (
	"bufio"
	"io/fs"
	"math"
	"path"
	"strconv"
	"strings"
	"time"
)

// System reads machine facts and live readings from a filesystem root, so
// tests can feed fake /proc and /sys trees. On platforms without those files
// every reading falls back to the platform probe or to "unknown" (zero).
type System struct {
	// Root is the filesystem containing proc/ and sys/ (normally "/").
	Root fs.FS
	// NumCPU is the CPU count available to the process before cgroup quotas.
	NumCPU int
}

// CgroupLimits are the resource limits of the process's cgroup. Zero means
// unlimited (or unknown).
type CgroupLimits struct {
	// CPU is the CPU quota in cores (quota ÷ period).
	CPU float64
	// Memory is the memory limit in bytes.
	Memory int64
}

// unlimitedV1 is the threshold above which a cgroup v1 memory limit means
// "no limit" (the kernel reports PAGE_COUNTER_MAX rounded to pages).
const unlimitedV1 = int64(1) << 62

// Cgroup returns the effective limits: the tightest over the process's
// cgroup and its ancestors (v2), or the v1 cpu and memory controllers.
func (s System) Cgroup() CgroupLimits {
	if _, err := fs.Stat(s.Root, "sys/fs/cgroup/cgroup.controllers"); err == nil {
		return s.cgroupV2()
	}
	return s.cgroupV1()
}

// selfCgroup parses proc/self/cgroup into controller list → path.
func (s System) selfCgroup() map[string]string {
	out := map[string]string{}
	b, err := fs.ReadFile(s.Root, "proc/self/cgroup")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 {
			out[parts[1]] = parts[2]
		}
	}
	return out
}

func (s System) cgroupV2() CgroupLimits {
	var lim CgroupLimits
	rel := strings.Trim(s.selfCgroup()[""], "/")
	// Walk from the process's cgroup to the root of the mount. In a
	// container with a cgroup namespace the process sits at the root.
	for dir := path.Join("sys/fs/cgroup", rel); ; dir = path.Dir(dir) {
		if cpu := readCPUMax(s.Root, path.Join(dir, "cpu.max")); cpu > 0 && (lim.CPU == 0 || cpu < lim.CPU) {
			lim.CPU = cpu
		}
		if mem := readLimit(s.Root, path.Join(dir, "memory.max")); mem > 0 && (lim.Memory == 0 || mem < lim.Memory) {
			lim.Memory = mem
		}
		if dir == "sys/fs/cgroup" || dir == "." || dir == "/" {
			return lim
		}
	}
}

// v1Dir finds the directory of a v1 controller, preferring the process's own
// cgroup and falling back to the hierarchy root (which is what a container
// sees without a cgroup namespace).
func (s System) v1Dir(controller, file string) string {
	for list, p := range s.selfCgroup() {
		for _, c := range strings.Split(list, ",") {
			if c != controller {
				continue
			}
			for _, mount := range []string{list, controller} {
				base := path.Join("sys/fs/cgroup", mount)
				for _, dir := range []string{path.Join(base, strings.Trim(p, "/")), base} {
					if _, err := fs.Stat(s.Root, path.Join(dir, file)); err == nil {
						return dir
					}
				}
			}
		}
	}
	for _, dir := range []string{"sys/fs/cgroup/" + controller, "sys/fs/cgroup/cpu,cpuacct"} {
		if _, err := fs.Stat(s.Root, path.Join(dir, file)); err == nil {
			return dir
		}
	}
	return ""
}

func (s System) cgroupV1() CgroupLimits {
	var lim CgroupLimits
	if dir := s.v1Dir("cpu", "cpu.cfs_quota_us"); dir != "" {
		quota := readInt(s.Root, path.Join(dir, "cpu.cfs_quota_us"))
		period := readInt(s.Root, path.Join(dir, "cpu.cfs_period_us"))
		if quota > 0 && period > 0 {
			lim.CPU = float64(quota) / float64(period)
		}
	}
	if dir := s.v1Dir("memory", "memory.limit_in_bytes"); dir != "" {
		if m := readInt(s.Root, path.Join(dir, "memory.limit_in_bytes")); m > 0 && m < unlimitedV1 {
			lim.Memory = m
		}
	}
	return lim
}

// UsableCores is the CPU count honouring a cgroup quota, rounded down, at
// least 1.
func (s System) UsableCores() int {
	n := s.NumCPU
	if n <= 0 {
		n = 1
	}
	if q := s.Cgroup().CPU; q > 0 && int(math.Floor(q)) < n {
		n = max(1, int(math.Floor(q)))
	}
	return n
}

// MemoryLimit is physical memory, lowered to the cgroup limit if tighter; 0
// if neither is known.
func (s System) MemoryLimit() int64 {
	phys := s.physicalMemory()
	cg := s.Cgroup().Memory
	switch {
	case phys == 0:
		return cg
	case cg > 0 && cg < phys:
		return cg
	}
	return phys
}

func (s System) physicalMemory() int64 {
	if kb := s.procField("proc/meminfo", "MemTotal"); kb > 0 {
		return kb * kib
	}
	return platformMemory()
}

// CPUModel is the processor's marketing name, if the platform says.
func (s System) CPUModel() string {
	if b, err := fs.ReadFile(s.Root, "proc/cpuinfo"); err == nil {
		for _, key := range []string{"model name", "Hardware", "Model", "cpu model"} {
			for _, line := range strings.Split(string(b), "\n") {
				k, v, ok := strings.Cut(line, ":")
				if ok && strings.TrimSpace(k) == key && strings.TrimSpace(v) != "" {
					return strings.Join(strings.Fields(v), " ")
				}
			}
		}
	}
	return platformCPUModel()
}

// Load1 is the one-minute load average, or 0.
func (s System) Load1() float64 {
	b, err := fs.ReadFile(s.Root, "proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// CPUThrottled is the cumulative time the cgroup was throttled by its CPU
// quota.
func (s System) CPUThrottled() time.Duration {
	if _, err := fs.Stat(s.Root, "sys/fs/cgroup/cgroup.controllers"); err == nil {
		rel := strings.Trim(s.selfCgroup()[""], "/")
		for _, dir := range []string{path.Join("sys/fs/cgroup", rel), "sys/fs/cgroup"} {
			if us := s.statField(path.Join(dir, "cpu.stat"), "throttled_usec"); us >= 0 {
				return time.Duration(us) * time.Microsecond
			}
		}
		return 0
	}
	if dir := s.v1Dir("cpu", "cpu.stat"); dir != "" {
		if ns := s.statField(path.Join(dir, "cpu.stat"), "throttled_time"); ns >= 0 {
			return time.Duration(ns)
		}
	}
	return 0
}

// RSS is a process's resident set size in bytes, or 0 if unknown.
func (s System) RSS(pid int) int64 {
	return s.procField("proc/"+strconv.Itoa(pid)+"/status", "VmRSS") * kib
}

// procField reads a "Key:   123 kB" line; -1 values become 0.
func (s System) procField(file, key string) int64 {
	f, err := s.Root.Open(file)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && k == key {
			fields := strings.Fields(v)
			if len(fields) > 0 {
				n, _ := strconv.ParseInt(fields[0], 10, 64)
				return max(n, 0)
			}
		}
	}
	return 0
}

// statField reads a "key value" line of a cgroup stat file, or -1.
func (s System) statField(file, key string) int64 {
	b, err := fs.ReadFile(s.Root, file)
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == key {
			n, err := strconv.ParseInt(f[1], 10, 64)
			if err == nil {
				return n
			}
		}
	}
	return -1
}

// readCPUMax parses cgroup v2 cpu.max ("max 100000" or "200000 100000")
// into cores; 0 means unlimited or unreadable.
func readCPUMax(root fs.FS, file string) float64 {
	b, err := fs.ReadFile(root, file)
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || f[0] == "max" {
		return 0
	}
	quota, err1 := strconv.ParseFloat(f[0], 64)
	period, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return quota / period
}

// readLimit parses a byte limit file ("max" or a number); 0 means unlimited.
func readLimit(root fs.FS, file string) int64 {
	n := readInt(root, file)
	if n <= 0 {
		return 0
	}
	return n
}

// readInt parses a file holding one integer; "max" and errors yield 0.
func readInt(root fs.FS, file string) int64 {
	b, err := fs.ReadFile(root, file)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
