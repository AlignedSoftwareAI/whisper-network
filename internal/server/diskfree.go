package server

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/deweysasser/whisper-network/internal/diskfree"
)

// FreeBytes returns the space available on the filesystem holding path.
func FreeBytes(path string) (int64, error) { return diskfree.Free(path) }

// TotalBytes returns the size of the filesystem holding path.
func TotalBytes(path string) (int64, error) { return diskfree.Total(path) }

// DiskCeiling is a limit on a filesystem's used space: a percentage of its
// size, or an absolute number of bytes. The zero value means the default.
type DiskCeiling struct {
	Percent float64 // 0 < Percent <= 100, or 0 when Bytes is used
	Bytes   int64
}

// ParseDiskCeiling parses "80%" or a size such as "200GiB".
func ParseDiskCeiling(s string) (DiskCeiling, error) {
	s = strings.TrimSpace(s)
	if p, ok := strings.CutSuffix(s, "%"); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || f <= 0 || f > 100 {
			return DiskCeiling{}, fmt.Errorf("disk ceiling %q: a percentage must be above 0 and at most 100", s)
		}
		return DiskCeiling{Percent: f}, nil
	}
	n, err := ParseByteSize(s)
	if err != nil || n <= 0 {
		return DiskCeiling{}, fmt.Errorf("disk ceiling %q: want a percentage (80%%) or a size (200GiB)", s)
	}
	return DiskCeiling{Bytes: n}, nil
}

// Limit returns the ceiling in bytes for a filesystem of total bytes.
func (c DiskCeiling) Limit(total int64) int64 {
	if c.Percent > 0 {
		return int64(float64(total) * c.Percent / 100)
	}
	return c.Bytes
}

func (c DiskCeiling) String() string {
	if c.Percent > 0 {
		return strconv.FormatFloat(c.Percent, 'f', -1, 64) + "%"
	}
	return strconv.FormatInt(c.Bytes, 10) + " bytes"
}
