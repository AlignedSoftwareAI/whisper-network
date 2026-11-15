//go:build linux

package sandbox

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// I/O priority constants from linux/ioprio.h.
const (
	ioprioWhoProcess = 1
	ioprioClassShift = 13
	ioprioClassIdle  = 3
)

// setPriority lowers the calling thread's CPU and I/O priority. Failures are
// returned as warnings: priority is a courtesy to the host, not a security
// control, and some container runtimes forbid these calls.
func setPriority(spec Spec) (warnings []string) {
	warn := func(err error) { warnings = append(warnings, err.Error()) }
	if err := setNice(spec.Nice); err != nil {
		warn(err)
	}
	if spec.SchedIdle {
		attr := unix.SchedAttr{Size: unix.SizeofSchedAttr, Policy: unix.SCHED_IDLE}
		if err := unix.SchedSetAttr(0, &attr, 0); err != nil {
			warn(fmt.Errorf("sched_setattr SCHED_IDLE: %w", err))
		}
	}
	if spec.IOPrioIdle {
		if err := ioprioSet(ioprioClassIdle << ioprioClassShift); err != nil {
			warn(fmt.Errorf("ioprio_set idle: %w", err))
		}
	}
	return warnings
}

// ioprioSet calls ioprio_set(2) for the calling thread.
func ioprioSet(value int) error {
	_, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, 0, uintptr(value))
	if errno != 0 {
		return errno
	}
	return nil
}

// ioprioGet calls ioprio_get(2) for the calling thread.
func ioprioGet() (int, error) {
	r, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}
