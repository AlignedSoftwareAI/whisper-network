package program

import (
	"os"
	"syscall"
)

// maxRSS is a finished child's peak resident set in bytes (Linux reports KiB).
func maxRSS(ps *os.ProcessState) int64 {
	if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
		return ru.Maxrss * 1024
	}
	return 0
}
