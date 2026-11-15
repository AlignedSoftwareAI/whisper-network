//go:build linux || darwin

package sandbox

import (
	"fmt"
	"syscall"
)

const rlimInfinity = ^uint64(0)

// lowerRlimit sets both the soft and hard limit of res to v, never raising a
// hard limit that is already lower. Lowering the hard limit makes the change
// irreversible for the child.
func lowerRlimit(res int, name string, v uint64) error {
	var cur syscall.Rlimit
	if err := syscall.Getrlimit(res, &cur); err != nil {
		return fmt.Errorf("getrlimit %s: %w", name, err)
	}
	if cur.Max != rlimInfinity && cur.Max < v {
		v = cur.Max
	}
	if err := syscall.Setrlimit(res, &syscall.Rlimit{Cur: v, Max: v}); err != nil {
		return fmt.Errorf("setrlimit %s: %w", name, err)
	}
	return nil
}

// setNice sets the nice value of the calling thread.
func setNice(n int) error {
	if n == 0 {
		return nil
	}
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, n); err != nil {
		return fmt.Errorf("setpriority: %w", err)
	}
	return nil
}
