package obs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m))
	return m
}

func TestLoggerCarriesModeAndEvent(t *testing.T) {
	var buf bytes.Buffer
	base := zerolog.New(&buf)
	l := NewLogger(base, ModeServer)
	Info(&l, EventJobCreated).Str(FieldJobID, "j1").Str(FieldQueue, "main").Msg("job created")

	m := decode(t, &buf)
	assert.Equal(t, "server", m["mode"])
	assert.Equal(t, "job.created", m["event"])
	assert.Equal(t, "j1", m["job_id"])
	assert.Equal(t, "info", m["level"])
}

func TestWithWorkerAndLevels(t *testing.T) {
	var buf bytes.Buffer
	l := WithWorker(NewLogger(zerolog.New(&buf), ModeWorker), "w9")
	Warn(&l, EventLeaseExpired).Msg("x")
	m := decode(t, &buf)
	assert.Equal(t, "worker", m["mode"])
	assert.Equal(t, "w9", m["worker_id"])
	assert.Equal(t, "warn", m["level"])
}

func TestEventNamesAreDottedAndUnique(t *testing.T) {
	events := []Event{
		EventHTTPRequest, EventJobCreated, EventJobPrepared, EventJobDone, EventJobFailed, EventJobDeleted,
		EventJobPurged, EventTaskLeased, EventLeaseExpired, EventLeaseReleased, EventTaskDone, EventTaskFailed,
		EventWorkerRegistered, EventWorkerApproved, EventWorkerPaused, EventWorkerResumed, EventWorkerRejected,
		EventWorkerSandbox, EventAuthDenied, EventAdminChange, EventReaperRun,
	}
	seen := map[Event]bool{}
	for _, e := range events {
		assert.Contains(t, string(e), ".")
		assert.False(t, seen[e], "duplicate %s", e)
		seen[e] = true
	}
}

func TestIsForbiddenField(t *testing.T) {
	for _, k := range []string{"token", "Password", "Authorization", "cookie", "Set-Cookie", "prompt",
		"transcript", "metadata", "client_token", "request.metadata", "X-Authorization"} {
		assert.True(t, IsForbiddenField(k), k)
	}
	for _, k := range []string{"token_len", "transcript_bytes", "metadata_size", "job_id", "tokens_per_s_x",
		"user", "error", "", "event"} {
		assert.False(t, IsForbiddenField(k), k)
	}
}

func TestForbiddenFieldsReturnsCopy(t *testing.T) {
	f := ForbiddenFields()
	assert.ElementsMatch(t, []string{"token", "password", "authorization", "cookie", "prompt", "transcript", "metadata"}, f)
	f[0] = "mutated"
	assert.NotEqual(t, "mutated", ForbiddenFields()[0])
}

func TestFieldNamesAreNotForbidden(t *testing.T) {
	for _, f := range []string{FieldEvent, FieldMode, FieldJobID, FieldTaskSeq, FieldTaskKind, FieldLeaseID,
		FieldQueue, FieldWeight, FieldWorkerID, FieldClientID, FieldUser, FieldModel, FieldDurationMS,
		FieldAudioSeconds, FieldBytes, FieldOutcome, FieldError, FieldStderrTail, FieldPrincipal, FieldSource} {
		assert.False(t, IsForbiddenField(f), f)
	}
}

func TestStderrTail(t *testing.T) {
	assert.Equal(t, "short", StderrTail("short"))

	long := strings.Repeat("é", MaxStderrTail) // two bytes per rune
	tail := StderrTail(long)
	assert.LessOrEqual(t, len(tail), MaxStderrTail)
	assert.Greater(t, len(tail), MaxStderrTail-4)
	assert.True(t, utf8.ValidString(tail))
	assert.True(t, strings.HasSuffix(long, tail), "keeps the end of the output")
}
