package ui

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/store"
)

// TestFailedLoginAudited: each failed sign-in is one auth.login_failed
// entry; an unknown username is not stored (it may be a mistyped password).
func TestFailedLoginAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("alice")
	b := e.browser()
	r, _ := b.submitLogin("alice", "not-her-password", "")
	require.Equal(t, http.StatusUnauthorized, r.StatusCode)
	r, _ = b.submitLogin("correct-horse-battery", "whatever-password", "")
	require.Equal(t, http.StatusUnauthorized, r.StatusCode)

	es, err := e.st.QueryAudit(e.ctx, store.AuditFilter{ActionPrefix: "auth.login_failed"})
	require.NoError(t, err)
	require.Len(t, es, 2)
	assert.Equal(t, "", es[0].Target, "unknown usernames are dropped")
	assert.Equal(t, "user:alice", es[1].Target)
	for _, a := range es {
		assert.NotContains(t, a.Detail, "not-her-password")
		assert.NotContains(t, a.Detail, "correct-horse-battery")
	}
}
