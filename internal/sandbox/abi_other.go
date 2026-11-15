//go:build !linux

package sandbox

// landlockABI is 0 where Landlock does not exist.
func landlockABI() int { return 0 }
