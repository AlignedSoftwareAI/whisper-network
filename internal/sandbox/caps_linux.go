//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// capSetPCap is CAP_SETPCAP, needed to shrink the bounding set.
const capSetPCap = 8

// dropCapabilities makes sure the exec'd child gains no capabilities, even
// when sandbox-exec runs as root: it empties the bounding set (when this
// process may), the inheritable set and the ambient set. It must run before
// the uid changes, because dropping the bounding set needs CAP_SETPCAP.
//
// An unprivileged process has no capabilities to pass on; it cannot shrink
// its bounding set, and does not need to: execve of a file without file
// capabilities gives a non-root uid nothing, and no_new_privs makes the
// kernel ignore file capabilities and set-id bits.
func dropCapabilities() error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capget: %w", err)
	}
	if data[0].Effective&(1<<capSetPCap) != 0 {
		last, err := lastCap()
		if err != nil {
			return err
		}
		for c := 0; c <= last; c++ {
			if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
				return fmt.Errorf("prctl(PR_CAPBSET_DROP, %d): %w", c, err)
			}
		}
	} else if os.Geteuid() == 0 {
		return errors.New("running as root without CAP_SETPCAP: cannot drop the capability bounding set")
	}
	if data[0].Inheritable != 0 || data[1].Inheritable != 0 {
		data[0].Inheritable, data[1].Inheritable = 0, 0
		if err := unix.Capset(&hdr, &data[0]); err != nil {
			return fmt.Errorf("capset: %w", err)
		}
	}
	// EINVAL: a kernel older than 4.3 has no ambient set to clear.
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("prctl(PR_CAP_AMBIENT_CLEAR_ALL): %w", err)
	}
	return nil
}

// lastCap is the highest capability number the kernel knows.
func lastCap() (int, error) {
	b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		// CAP_CHECKPOINT_RESTORE (40) is the highest as of Linux 7.0;
		// dropping unknown numbers is harmless (EINVAL is ignored).
		return 63, nil //nolint:nilerr // fall back to every possible bit
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("cap_last_cap: %w", err)
	}
	return n, nil
}
