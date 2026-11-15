//go:build !linux

package sandbox

import "errors"

// CountTasks is not available off Linux, where RLIMIT_NPROC is not applied.
func CountTasks(int) (uint64, error) {
	return 0, errors.New("counting a uid's tasks is supported only on Linux")
}
