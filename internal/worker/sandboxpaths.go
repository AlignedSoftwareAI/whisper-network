package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// systemReadOnly are the system paths every child may read: shared
// libraries and the dynamic loader's configuration.
var systemReadOnly = []string{"/usr", "/lib", "/lib64", "/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d"}

// childSystemFiles are harmless system files tools commonly probe.
var childSystemFiles = []string{"/proc/cpuinfo", "/sys/devices/system/cpu", "/dev/null", "/dev/urandom"}

// toolReadOnly returns what a child running tool may read besides the
// system paths: the binary itself and, when the binary lives outside the
// system paths, its directory, because whisper.cpp's shared libraries and
// ggml's CPU backends sit next to the executable (/opt/whisper in the
// image). That directory is granted whole, so it must be dedicated to the
// tool: one that contains the worker's state (credentials, other tasks'
// work) or lies under the home directory is refused, as is a state
// directory under any read-only path.
func toolReadOnly(tool, stateDir, home string) ([]string, error) {
	real, err := filepath.EvalSymlinks(tool)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", tool, err)
	}
	ro := []string{real}
	if tool != real {
		ro = append(ro, tool)
	}
	dir := filepath.Dir(real)
	if !underAny(dir, systemReadOnly) {
		home = canonical(home)
		switch {
		case dir == "/":
			return nil, fmt.Errorf("%s: a tool in / would expose the whole filesystem to the sandbox; install it in a dedicated directory such as /opt/whisper", tool)
		case home != "" && home != "/" && within(dir, home):
			return nil, fmt.Errorf("%s: the sandbox grants read access to the tool's directory %s, which is under the home directory %s; install the tool in a dedicated directory such as /opt/whisper", tool, dir, home)
		}
		ro = append(ro, dir)
	}
	if err := checkStateOutside(stateDir, ro); err != nil {
		return nil, fmt.Errorf("%s: %w", tool, err)
	}
	return ro, nil
}

// checkStateOutside refuses a state directory that children could read
// through one of the read-only paths.
func checkStateOutside(stateDir string, ro []string) error {
	state := canonical(stateDir)
	for _, p := range ro {
		if within(state, canonical(p)) {
			return fmt.Errorf("the state directory %s is inside %s, which sandboxed children may read; move it", stateDir, p)
		}
	}
	return nil
}

// canonical returns p absolute, with symlinks resolved where it exists.
func canonical(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	return filepath.Clean(p)
}

// within reports whether path is root or lies below it.
func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func underAny(path string, roots []string) bool {
	for _, r := range roots {
		if within(path, r) {
			return true
		}
	}
	return false
}

// homeDir is the home directory tool directories must stay out of.
func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
