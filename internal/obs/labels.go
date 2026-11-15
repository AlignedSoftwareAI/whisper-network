package obs

import (
	"fmt"
	"sort"
	"strings"
)

// EscapeLabelValue escapes v for use inside a double-quoted Prometheus label
// value: backslash, double quote and newline.
func EscapeLabelValue(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// series formats a metric name with labels, given as alternating key and
// value strings: series("m", "a", "1") is `m{a="1"}`. Values are escaped.
func series(name string, kv ...string) string {
	if len(kv) == 0 {
		return name
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[i])
		b.WriteString(`="`)
		b.WriteString(EscapeLabelValue(kv[i+1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// FormatLabels renders labels as the comma-separated `k="v"` list that the
// push API's extra-labels option takes, sorted by key.
func FormatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	kv := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		kv = append(kv, k, labels[k])
	}
	s := series("x", kv...)
	return strings.TrimSuffix(strings.TrimPrefix(s, "x{"), "}")
}

// ParseLabels parses the --metrics-labels flag: comma-separated k=v pairs
// such as `env=prod,dc=home`. Keys must be valid label names.
func ParseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		if !ok || !validLabelName(k) {
			return nil, fmt.Errorf("metrics label %q: want name=value", part)
		}
		out[k] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out, nil
}

func validLabelName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}
