//go:build darwin

package sandbox

import "syscall"

// apply sets what darwin supports portably: nice, RLIMIT_FSIZE and
// RLIMIT_NOFILE. RLIMIT_AS is not enforced by macOS, and there is no Landlock.
func apply(spec Spec, _ string) ([]string, error) {
	var warnings []string
	if err := setNice(spec.Nice); err != nil {
		warnings = append(warnings, err.Error())
	}
	if spec.FileSize > 0 {
		if err := lowerRlimit(syscall.RLIMIT_FSIZE, "RLIMIT_FSIZE", spec.FileSize); err != nil {
			return warnings, err
		}
	}
	if spec.OpenFiles > 0 {
		if err := lowerRlimit(syscall.RLIMIT_NOFILE, "RLIMIT_NOFILE", spec.OpenFiles); err != nil {
			return warnings, err
		}
	}
	if spec.Confines() || spec.AddressSpace > 0 || spec.Processes > 0 || spec.ProcessLimit > 0 {
		warnings = append(warnings, "filesystem, network, process and address-space limits are not available on darwin")
	}
	return warnings, nil
}
