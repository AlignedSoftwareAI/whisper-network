//go:build !darwin

package worker

// On Linux everything comes from /proc and /sys through System.Root; other
// platforms have no probe and need --memory.

func platformMemory() int64    { return 0 }
func platformCPUModel() string { return "" }
