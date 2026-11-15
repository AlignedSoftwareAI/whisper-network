package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

func TestTokenGenerateParse(t *testing.T) {
	cases := []struct {
		name string
		gen  func() (string, error)
		pfx  string
		kind PrincipalKind
	}{
		{"client", NewClientToken, api.ClientTokenPrefix, KindClient},
		{"worker", NewWorkerToken, api.WorkerTokenPrefix, KindWorker},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := tc.gen()
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(tok, tc.pfx))
			assert.Len(t, tok, len(tc.pfx)+tokenBodyLen)

			kind, hash, err := ParseToken(tok)
			require.NoError(t, err)
			assert.Equal(t, tc.kind, kind)
			assert.Equal(t, HashToken(tok), hash)
			assert.Len(t, hash, 64)

			other, _ := tc.gen()
			assert.NotEqual(t, tok, other)
		})
	}
}

func TestHashTokenKnownVector(t *testing.T) {
	assert.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", HashToken("abc"))
}

func TestParseTokenMalformed(t *testing.T) {
	good, _ := NewClientToken()
	body := good[len(api.ClientTokenPrefix):]
	bad := []string{
		"", "wnc_", "wnw_", "wnc", good[:len(good)-1], good + "0",
		"WNC_" + body, "wns_" + body, "Bearer " + good, " " + good, good + " ", good + "\n",
		"wnc_" + strings.Repeat("-", tokenBodyLen), "wnc_" + body[:10] + "é" + body[12:],
		"wnc_" + strings.Repeat("a", 1000), "wnc_\x00" + body[1:],
	}
	for _, b := range bad {
		_, h, err := ParseToken(b)
		assert.ErrorIs(t, err, ErrBadToken, "%q", b)
		assert.Empty(t, h)
	}
}

func FuzzParseToken(f *testing.F) {
	c, _ := NewClientToken()
	w, _ := NewWorkerToken()
	for _, s := range []string{c, w, "", "wnc_", "wnw_0", "x", "wnc_" + strings.Repeat("z", tokenBodyLen)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		kind, hash, err := ParseToken(s)
		if err != nil {
			if kind != "" || hash != "" {
				t.Fatalf("error with non-empty results for %q", s)
			}
			return
		}
		if kind != KindClient && kind != KindWorker {
			t.Fatalf("bad kind %q", kind)
		}
		if len(s) != 4+tokenBodyLen || hash != HashToken(s) {
			t.Fatalf("accepted %q", s)
		}
	})
}

func TestSessionAndCSRF(t *testing.T) {
	id, err := NewSessionID()
	require.NoError(t, err)
	h, err := ParseSessionID(id)
	require.NoError(t, err)
	assert.Equal(t, HashToken(id), h)

	// a session id is not a bearer token, and vice versa
	_, _, err = ParseToken(id)
	assert.ErrorIs(t, err, ErrBadToken)
	c, _ := NewClientToken()
	_, err = ParseSessionID(c)
	assert.ErrorIs(t, err, ErrBadToken)
	_, err = ParseSessionID("wns_short")
	assert.ErrorIs(t, err, ErrBadToken)

	csrf, err := NewCSRFToken()
	require.NoError(t, err)
	assert.Len(t, csrf, tokenBodyLen)
	other, _ := NewCSRFToken()
	assert.NotEqual(t, csrf, other)
}

func TestEqual(t *testing.T) {
	assert.True(t, Equal("abc", "abc"))
	assert.False(t, Equal("abc", "abd"))
	assert.False(t, Equal("abc", "abcd"))
	assert.True(t, Equal("", ""))
}
