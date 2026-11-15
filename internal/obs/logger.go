package obs

import (
	"strings"
	"unicode/utf8"

	"github.com/rs/zerolog"
)

// Mode says which part of the system a logger speaks for. It is a VictoriaLogs
// stream field, so it must stay low-cardinality.
type Mode string

// The program modes.
const (
	ModeServer Mode = "server"
	ModeWorker Mode = "worker"
)

// NewLogger returns base scoped to mode: every line it writes carries
// mode=<mode>.
func NewLogger(base zerolog.Logger, mode Mode) zerolog.Logger {
	return base.With().Str(FieldMode, string(mode)).Logger()
}

// WithWorker returns l with worker_id attached to every line. Call it once the
// worker's ID is known.
func WithWorker(l zerolog.Logger, workerID string) zerolog.Logger {
	return l.With().Str(FieldWorkerID, workerID).Logger()
}

// Debug starts a debug-level log event named ev.
func Debug(l *zerolog.Logger, ev Event) *zerolog.Event { return l.Debug().Str(FieldEvent, string(ev)) }

// Info starts an info-level log event named ev.
func Info(l *zerolog.Logger, ev Event) *zerolog.Event { return l.Info().Str(FieldEvent, string(ev)) }

// Warn starts a warn-level log event named ev.
func Warn(l *zerolog.Logger, ev Event) *zerolog.Event { return l.Warn().Str(FieldEvent, string(ev)) }

// Error starts an error-level log event named ev.
func Error(l *zerolog.Logger, ev Event) *zerolog.Event { return l.Error().Str(FieldEvent, string(ev)) }

// MaxStderrTail is the longest stderr_tail value that may be logged.
const MaxStderrTail = 4 << 10

// StderrTail returns the last MaxStderrTail bytes of s, trimmed to a valid
// UTF-8 boundary, for the stderr_tail field.
func StderrTail(s string) string {
	if len(s) <= MaxStderrTail {
		return s
	}
	s = s[len(s)-MaxStderrTail:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return strings.ToValidUTF8(s, "")
}
