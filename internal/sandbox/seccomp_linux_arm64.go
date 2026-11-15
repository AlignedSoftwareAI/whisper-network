//go:build linux && arm64

package sandbox

import "golang.org/x/sys/unix"

// seccompArch is the AUDIT_ARCH value the filter accepts.
const seccompArch = unix.AUDIT_ARCH_AARCH64
