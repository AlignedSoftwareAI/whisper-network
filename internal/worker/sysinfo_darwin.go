//go:build darwin

package worker

import "golang.org/x/sys/unix"

func platformMemory() int64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(n)
}

func platformCPUModel() string {
	s, err := unix.Sysctl("machdep.cpu.brand_string")
	if err != nil {
		return ""
	}
	return s
}
