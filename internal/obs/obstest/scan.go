// Package obstest checks captured logs against the never-log rules of
// DESIGN.md §10.1. Server and worker packages use it in tests.
package obstest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/obs"
)

// Violation is one forbidden key or secret-looking value in a log line.
type Violation struct {
	Line   int    // 1-based line number
	Path   string // key path, e.g. "request.headers.authorization"
	Reason string
}

func (v Violation) String() string { return fmt.Sprintf("line %d: %s: %s", v.Line, v.Path, v.Reason) }

var (
	tokenRE = regexp.MustCompile(
		regexp.QuoteMeta(api.ClientTokenPrefix) + `[A-Za-z0-9_\-]{8,}|` +
			regexp.QuoteMeta(api.WorkerTokenPrefix) + `[A-Za-z0-9_\-]{8,}`)
	bearerRE = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=\-]{8,}`)
)

// Scan reads JSONL log output and reports every forbidden key (at any depth)
// and every string value that looks like a token: a wnc_/wnw_ credential or a
// bearer header value. A line that is not a JSON object is itself a violation,
// since the program must emit JSONL.
func Scan(jsonl io.Reader) ([]Violation, error) {
	var out []Violation
	sc := bufio.NewScanner(jsonl)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(line, &obj); err != nil {
			out = append(out, Violation{Line: n, Path: "-", Reason: "not a JSON object: " + err.Error()})
			continue
		}
		walk(n, "", obj, &out)
	}
	return out, sc.Err()
}

func walk(line int, path string, v any, out *[]Violation) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if obs.IsForbiddenField(k) {
				*out = append(*out, Violation{line, p, "forbidden field name"})
			}
			walk(line, p, x[k], out)
		}
	case []any:
		for i, e := range x {
			walk(line, fmt.Sprintf("%s[%d]", path, i), e, out)
		}
	case string:
		switch {
		case tokenRE.MatchString(x):
			*out = append(*out, Violation{line, path, "value looks like a " + api.ClientTokenPrefix + "/" + api.WorkerTokenPrefix + " token"})
		case bearerRE.MatchString(x):
			*out = append(*out, Violation{line, path, "value looks like a bearer credential"})
		}
	}
}

// AssertClean fails t if the JSONL in captured breaks the never-log rules.
func AssertClean(t testing.TB, captured []byte) {
	t.Helper()
	vs, err := Scan(bytes.NewReader(captured))
	if err != nil {
		t.Fatalf("scanning logs: %v", err)
	}
	if len(vs) == 0 {
		return
	}
	var b strings.Builder
	for _, v := range vs {
		b.WriteString("\n  " + v.String())
	}
	t.Errorf("log output breaks the never-log rules:%s", b.String())
}

// Capture returns a logger scoped to mode that writes JSONL into the returned
// buffer, for use with AssertClean.
func Capture(mode obs.Mode) (zerolog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	l := zerolog.New(&buf).With().Timestamp().Logger()
	return obs.NewLogger(l, mode), &buf
}
