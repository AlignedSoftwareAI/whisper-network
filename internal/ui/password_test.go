package ui

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
)

// postFrom submits a form with the session's CSRF token as if from ip
// (the server trusts X-Forwarded-For in these tests).
func (b *browser) postFrom(ip, path string, form url.Values) (*http.Response, string) {
	b.e.t.Helper()
	form.Set("csrf", b.csrf)
	return b.do("POST", path, form, map[string]string{"X-Forwarded-For": ip})
}

func changeForm(current, pw string) url.Values {
	return url.Values{"current": {current}, "password": {pw}, "confirm": {pw}, "next": {"/ui/"}}
}

// auditSince returns "action actor target" for each audit entry after
// mark, oldest first, and the newest id.
func (e *env) auditSince(mark int64) ([]string, int64) {
	e.t.Helper()
	es, err := e.st.QueryAudit(e.ctx, store.AuditFilter{Limit: 1000})
	require.NoError(e.t, err)
	var out []string
	newest := mark
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].ID > mark {
			out = append(out, es[i].Action+" "+es[i].Actor+" "+es[i].Target)
			newest = max(newest, es[i].ID)
		}
	}
	return out, newest
}

// TestPasswordCheckRateLimit: checking the current password on
// /ui/password is a password guess, limited per user (across addresses)
// and per address (shared with sign-in).
func TestPasswordCheckRateLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *server.Config) { c.TrustProxy = true })

	t.Run("per user", func(t *testing.T) {
		e.user("alice")
		b := e.browser()
		resp, _ := b.do("POST", "/ui/login", url.Values{"username": {"alice"}, "password": {"alice-password"}},
			map[string]string{"X-Forwarded-For": "198.51.100.1"})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		b.refreshCSRF()
		for i := range loginBurst {
			resp, body := b.postFrom("203.0.113."+strconv.Itoa(i+1), "/ui/password",
				changeForm("not-alice-password", "alice-new-password"))
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Contains(t, body, "current password is incorrect")
		}
		resp, body := b.postFrom("203.0.113.200", "/ui/password", changeForm("alice-password", "alice-new-password"))
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "a fresh address does not reset the user's budget")
		assert.Contains(t, body, "Too many attempts")
		assert.Equal(t, "60", resp.Header.Get("Retry-After"))

		// Form errors that need no password check are not charged.
		resp, _ = b.postFrom("203.0.113.201", "/ui/password", changeForm("alice-password", "short"))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		e.clk.Advance(loginInterval)
		resp, _ = b.postFrom("203.0.113.202", "/ui/password", changeForm("alice-password", "alice-new-password"))
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "a token refills after the interval")
	})

	t.Run("per address", func(t *testing.T) {
		e.user("bob")
		b := e.browser()
		const ip = "192.0.2.50"
		resp, _ := b.do("POST", "/ui/login", url.Values{"username": {"bob"}, "password": {"bob-password"}},
			map[string]string{"X-Forwarded-For": ip})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		b.refreshCSRF()
		for range loginBurst - 1 { // the sign-in took one token from the address
			resp, _ := b.postFrom(ip, "/ui/password", changeForm("not-bob-password", "bob-new-password!"))
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		}
		resp, _ = b.postFrom(ip, "/ui/password", changeForm("bob-password", "bob-new-password!"))
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	})
}

// TestPasswordBusy: when password hashing is saturated, sign-in and
// password change answer 503 with Retry-After and a "busy" message rather
// than a generic error.
func TestPasswordBusy(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *server.Config) {
		c.BusyWait = 20 * time.Millisecond
		c.PasswordMemoryBudget = 1 // two slots
	})
	e.user("alice")
	b := e.login("alice")
	r1, err := e.srv.PasswordSlot(e.ctx)
	require.NoError(t, err)
	r2, err := e.srv.PasswordSlot(e.ctx)
	require.NoError(t, err)

	resp, body := e.browser().submitLogin("alice", "alice-password", "")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
	assert.Contains(t, body, "The server is busy")

	resp, body = b.post("/ui/password", changeForm("alice-password", "alice-new-password"))
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
	assert.Contains(t, body, "The server is busy")

	r1()
	r2()
	resp, _ = b.post("/ui/password", changeForm("alice-password", "alice-new-password"))
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
}

// TestSessionLifecycleAudited: UI sign-in, failed sign-in, password change
// and sign-out each write exactly their audit rows.
func TestSessionLifecycleAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("alice")
	_, mark := e.auditSince(0)
	b := e.browser()

	step := func(want []string, do func()) {
		t.Helper()
		do()
		var got []string
		got, mark = e.auditSince(mark)
		assert.Equal(t, want, got)
	}
	step([]string{"auth.login_failed anonymous user:alice"}, func() {
		resp, _ := b.submitLogin("alice", "wrong-password", "")
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
	step([]string{"auth.login user:alice user:alice"}, func() {
		resp, _ := b.submitLogin("alice", "alice-password", "")
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	})
	b.refreshCSRF()
	step(nil, func() {
		resp, _ := b.post("/ui/password", changeForm("not-alice-password", "alice-new-password"))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
	step([]string{"user.change_password user:alice user:alice", "auth.login user:alice user:alice"}, func() {
		resp, _ := b.post("/ui/password", changeForm("alice-password", "alice-new-password"))
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	})
	b.refreshCSRF()
	step([]string{"auth.logout user:alice user:alice"}, func() {
		resp, _ := b.post("/ui/logout", nil)
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	})
}
