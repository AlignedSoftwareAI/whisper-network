package server

import "github.com/deweysasser/whisper-network/internal/diskfree"

// FreeBytes returns the space available on the filesystem holding path.
func FreeBytes(path string) (int64, error) { return diskfree.Free(path) }
