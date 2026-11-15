package server

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseByteSize parses a size such as "1048576", "512MiB", "2GiB" or "1.5G".
// Both SI (k, M, G, T: powers of 1000) and IEC (KiB, MiB, GiB, TiB: powers
// of 1024) suffixes are accepted, case-insensitively; a trailing "B" is
// optional.
func ParseByteSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	i := strings.IndexFunc(t, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	num, unit := t, ""
	if i >= 0 {
		num, unit = t[:i], strings.ToLower(strings.TrimSpace(t[i:]))
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	mult := map[string]float64{
		"": 1, "b": 1,
		"k": 1e3, "kb": 1e3, "m": 1e6, "mb": 1e6, "g": 1e9, "gb": 1e9, "t": 1e12, "tb": 1e12,
		"ki": 1 << 10, "kib": 1 << 10, "mi": 1 << 20, "mib": 1 << 20,
		"gi": 1 << 30, "gib": 1 << 30, "ti": 1 << 40, "tib": 1 << 40,
	}[unit]
	if mult == 0 {
		return 0, fmt.Errorf("invalid size unit in %q", s)
	}
	v := f * mult
	if v > math.MaxInt64/2 {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return int64(v), nil
}

// ParseFactors parses "name=number,name=number" (the --class-weights and
// --model-factors flags). Every number must be positive.
func ParseFactors(s string) (map[string]float64, error) {
	out := map[string]float64{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if !ok || k == "" || err != nil || !(f > 0) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("invalid entry %q: want name=positive-number", part)
		}
		out[strings.TrimSpace(k)] = f
	}
	return out, nil
}
