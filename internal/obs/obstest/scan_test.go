package obstest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/obs"
)

func scan(t *testing.T, s string) []Violation {
	t.Helper()
	vs, err := Scan(strings.NewReader(s))
	require.NoError(t, err)
	return vs
}

func TestScanPassesCleanLogs(t *testing.T) {
	l, buf := Capture(obs.ModeServer)
	obs.Info(&l, obs.EventJobCreated).Str(obs.FieldJobID, "0193abc").Str(obs.FieldQueue, "main").
		Int("token_len", 40).Str(obs.FieldSource, "https://example.com/a.mp3").Msg("created")
	obs.Warn(&l, obs.EventTaskFailed).Str(obs.FieldError, "exit 1").Msg("failed")
	assert.Empty(t, scan(t, buf.String()))
	AssertClean(t, buf.Bytes())
}

func TestScanFindsForbiddenKeys(t *testing.T) {
	for _, key := range obs.ForbiddenFields() {
		t.Run(key, func(t *testing.T) {
			l, buf := Capture(obs.ModeWorker)
			l.Info().Str(key, "x").Msg("m")
			vs := scan(t, buf.String())
			require.Len(t, vs, 1)
			assert.Equal(t, key, vs[0].Path)
			assert.Equal(t, 1, vs[0].Line)
		})
	}
}

func TestScanFindsNestedKeysAndReportsLine(t *testing.T) {
	in := `{"event":"a"}` + "\n" + `{"req":{"headers":{"Authorization":"x"}},"list":[{"cookie":"c"}]}` + "\n"
	vs := scan(t, in)
	require.Len(t, vs, 2)
	assert.Equal(t, "list[0].cookie", vs[0].Path)
	assert.Equal(t, "req.headers.Authorization", vs[1].Path)
	assert.Equal(t, 2, vs[0].Line)
}

func TestScanFindsTokenLookingValues(t *testing.T) {
	for _, prefix := range []string{api.ClientTokenPrefix, api.WorkerTokenPrefix} {
		vs := scan(t, `{"_msg":"got `+prefix+`AbCd1234efGH5678 from client"}`)
		require.Len(t, vs, 1, prefix)
		assert.Equal(t, "_msg", vs[0].Path)
	}
	vs := scan(t, `{"error":"bad header Bearer abcdefghijklmnop"}`)
	require.Len(t, vs, 1)

	assert.Empty(t, scan(t, `{"note":"the wnc_ prefix identifies client tokens"}`), "prefix alone is not a token")
	assert.Empty(t, scan(t, `{"user":"bearer"}`))
}

func TestScanFlagsNonJSONLines(t *testing.T) {
	vs := scan(t, "plain text line\n")
	require.Len(t, vs, 1)
	assert.Contains(t, vs[0].Reason, "not a JSON object")
}

func TestScanIgnoresBlankLines(t *testing.T) {
	assert.Empty(t, scan(t, "\n\n"+`{"a":1}`+"\n\n"))
}

type recorder struct {
	testing.TB
	failed bool
	msg    string
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(f string, a ...any) {
	r.failed = true
	r.msg = strings.TrimSpace(strings.ReplaceAll(f, "%s", "")) + " "
	_ = a
}
func (r *recorder) Fatalf(f string, a ...any) { r.failed = true }

func TestAssertCleanFailsOnViolation(t *testing.T) {
	r := &recorder{TB: t}
	AssertClean(r, []byte(`{"password":"hunter2"}`))
	assert.True(t, r.failed)

	r = &recorder{TB: t}
	AssertClean(r, []byte(`{"event":"ok"}`))
	assert.False(t, r.failed)
}
