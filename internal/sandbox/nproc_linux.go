//go:build linux

package sandbox

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

// CountTasks returns how many tasks (threads) the real uid runs, as far as
// /proc shows. RLIMIT_NPROC is checked against the kernel's per-uid count,
// which includes processes in other PID namespaces (other containers sharing
// the uid) that /proc here may not list.
func CountTasks(uid int) (uint64, error) {
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	var n uint64
	for _, d := range dirs {
		if _, err := strconv.Atoi(d.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", d.Name(), "status"))
		if err != nil {
			continue // exited, or hidden
		}
		var ruid int = -1
		var threads uint64
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			line := sc.Bytes()
			switch {
			case bytes.HasPrefix(line, []byte("Uid:")):
				if f := bytes.Fields(line[4:]); len(f) > 0 {
					ruid, _ = strconv.Atoi(string(f[0]))
				}
			case bytes.HasPrefix(line, []byte("Threads:")):
				threads, _ = strconv.ParseUint(string(bytes.TrimSpace(line[8:])), 10, 64)
			}
		}
		if ruid == uid {
			n += threads
		}
	}
	return n, nil
}

// countTasks is CountTasks, with 0 when /proc is unreadable.
func countTasks(uid int) uint64 {
	n, _ := CountTasks(uid)
	return n
}
