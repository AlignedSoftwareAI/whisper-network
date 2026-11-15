package worker

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/deweysasser/whisper-network/internal/api"
)

// whisperOutput is the subset of whisper-cli's -oj output that the worker
// uses. The writer is output_json in examples/cli/cli.cpp (whisper.cpp
// v1.9.5); "offsets" are milliseconds (segment t0/t1 are centiseconds,
// written ×10), "timestamps" are the same values formatted as text.
type whisperOutput struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Transcription []struct {
		Offsets struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"offsets"`
		Text string `json:"text"`
	} `json:"transcription"`
}

// parseWhisperJSON converts whisper-cli JSON to segments relative to the task
// start, clamped to [0, durationMS] (durationMS <= 0 disables the clamp),
// with start times made monotonic, text trimmed, and empty segments dropped.
// It returns the detected language as well.
func parseWhisperJSON(raw []byte, durationMS int64) ([]api.Segment, string, error) {
	var out whisperOutput
	if err := json.Unmarshal(sanitizeWhisperJSON(raw), &out); err != nil {
		return nil, "", fmt.Errorf("parsing whisper-cli output: %w", err)
	}
	segs := make([]api.Segment, 0, len(out.Transcription))
	var prevStart int64
	for _, t := range out.Transcription {
		text := strings.TrimSpace(strings.ToValidUTF8(t.Text, "�"))
		if text == "" {
			continue
		}
		start, end := clampMS(t.Offsets.From, durationMS), clampMS(t.Offsets.To, durationMS)
		start = max(start, prevStart)
		end = max(end, start)
		segs = append(segs, api.Segment{StartMS: start, EndMS: end, Text: text})
		prevStart = start
	}
	return segs, out.Result.Language, nil
}

func clampMS(v, limit int64) int64 {
	v = max(v, 0)
	if limit > 0 {
		v = min(v, limit)
	}
	return v
}

// sanitizeWhisperJSON makes whisper-cli's hand-written JSON parseable.
// output_json escapes only '"' and '\\' (escape_double_quotes_and_backslashes
// in cli.cpp), so control characters such as a raw tab or newline can appear
// inside strings, which encoding/json rejects. Inside string literals they
// are replaced with a space. Invalid UTF-8 is left to encoding/json, which
// substitutes U+FFFD.
func sanitizeWhisperJSON(b []byte) []byte {
	out := make([]byte, len(b))
	inString, escaped := false, false
	for i, c := range b {
		out[i] = c
		switch {
		case !inString:
			inString = c == '"'
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inString = false
		case c < 0x20:
			out[i] = ' '
		}
	}
	return out
}
