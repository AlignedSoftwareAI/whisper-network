//go:build linux && !amd64 && !arm64

package sandbox

import "golang.org/x/sys/unix"

// buildSeccomp has no syscall table for this architecture; restrictSeccomp
// then warns and the self-test reports the weaker sandbox.
func buildSeccomp(seccompRules) ([]unix.SockFilter, error) { return nil, errNoSeccompArch }
