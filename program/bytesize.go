package program

import (
	"fmt"
	"strconv"
	"strings"
)

// ByteSize is a byte count flag accepting plain numbers or binary/decimal
// suffixes: 512MiB, 1GiB, 6G, 800M.
type ByteSize int64

var byteUnits = []struct {
	suffix string
	mult   int64
}{
	{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
	{"kb", 1e3}, {"mb", 1e6}, {"gb", 1e9}, {"tb", 1e12},
	{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"t", 1 << 40},
	{"b", 1},
}

// UnmarshalText parses a size.
func (b *ByteSize) UnmarshalText(text []byte) error {
	s := strings.ToLower(strings.TrimSpace(string(text)))
	mult := int64(1)
	for _, u := range byteUnits {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.mult
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return fmt.Errorf("invalid size %q: want e.g. 512MiB, 1GiB", text)
	}
	*b = ByteSize(v * float64(mult))
	return nil
}

// String renders the size in MiB for help output.
func (b ByteSize) String() string {
	if b == 0 {
		return "0"
	}
	return fmt.Sprintf("%dMiB", int64(b)>>20)
}
