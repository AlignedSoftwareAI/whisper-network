//go:build !linux && !darwin

package program

import "os"

// maxRSS is unknown on this platform.
func maxRSS(*os.ProcessState) int64 { return 0 }
