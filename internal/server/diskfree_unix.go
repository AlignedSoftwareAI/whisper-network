//go:build !windows

package server

import "golang.org/x/sys/unix"

// FreeBytes returns the space available to unprivileged users on the
// filesystem holding path.
func FreeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	// The field types differ between platforms, hence the conversions.
	return int64(st.Bavail) * int64(st.Bsize), nil
}
