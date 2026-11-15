package ui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/server"
)

func TestLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("alice")

	t.Run("success", func(t *testing.T) {
		b := e.browser()
		resp, _ := b.submitLogin("alice", "alice-password", "")
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, "/ui/", resp.Header.Get("Location"))
		var sess *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == server.SessionCookieName {
				sess = c
			}
		}
		require.NotNil(t, sess)
		assert.True(t, sess.HttpOnly)
		assert.Equal(t, http.SameSiteLaxMode, sess.SameSite)
		resp, body := b.get("/ui/")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, "alice")
		// The login page skips itself when already signed in.
		resp, _ = b.get("/ui/login")
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	})

	t.Run("failures say nothing about which part was wrong", func(t *testing.T) {
		b := e.browser()
		r1, wrongPass := b.submitLogin("alice", "not-her-password", "")
		r2, noUser := b.submitLogin("mallory", "whatever-password", "")
		assert.Equal(t, http.StatusUnauthorized, r1.StatusCode)
		assert.Equal(t, http.StatusUnauthorized, r2.StatusCode)
		assert.Contains(t, wrongPass, "Invalid username or password.")
		assert.Contains(t, noUser, "Invalid username or password.")
		assert.Empty(t, r1.Cookies())
		resp, _ := b.get("/ui/")
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "no session")

		logs := string(e.logs.Bytes())
		assert.NotContains(t, logs, "not-her-password")
		assert.NotContains(t, logs, "whatever-password")
		assert.NotContains(t, logs, "mallory")
		denied := e.logs.events(t, obs.EventAuthDenied)
		require.NotEmpty(t, denied)
		assert.Equal(t, "bad credentials", denied[len(denied)-1][obs.FieldReason])
		assert.Equal(t, "/ui/login", denied[len(denied)-1][obs.FieldRoute])
	})

	t.Run("disabled users cannot sign in", func(t *testing.T) {
		e.user("dora")
		require.NoError(t, e.srv.Ops().DisableUser(e.ctx, server.CLIActor(), "dora"))
		resp, _ := e.browser().submitLogin("dora", "dora-password", "")
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}

func TestLoginRateLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("alice")
	b := e.browser()
	for range loginBurst {
		resp, _ := b.submitLogin("alice", "wrong-password!", "")
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	}
	resp, body := b.submitLogin("alice", "alice-password", "")
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "even the right password waits")
	assert.Contains(t, body, "Too many sign-in attempts")
	assert.Equal(t, "60", resp.Header.Get("Retry-After"))

	e.clk.Advance(loginInterval)
	resp, _ = b.submitLogin("alice", "alice-password", "")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "a token refills after the interval")
}

func TestForcedPasswordChange(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	b := e.browser()
	resp, _ := b.submitLogin("admin", bootstrapPassword, "/ui/audit")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/password?next=%2Fui%2Faudit", resp.Header.Get("Location"))

	// Every page sends the user back to the change form.
	for _, p := range []string{"/ui/", "/ui/users", "/ui/workers"} {
		resp, _ := b.get(p)
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode, p)
		assert.Equal(t, "/ui/password", resp.Header.Get("Location"), p)
	}
	resp, body := b.get("/ui/password?next=%2Fui%2Faudit")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "You must choose a new password")
	b.csrf = csrfRE.FindStringSubmatch(body)[1]

	change := func(current, pw, confirm string) (*http.Response, string) {
		return b.post("/ui/password", url.Values{"current": {current}, "password": {pw}, "confirm": {confirm},
			"next": {"/ui/audit"}})
	}
	resp, body = change(bootstrapPassword, "short", "short")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, body, "at least 12 characters")
	resp, body = change(bootstrapPassword, "a-brand-new-password", "a-different-password")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, body, "do not match")
	resp, body = change("not-the-current-one", "a-brand-new-password", "a-brand-new-password")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, body, "current password is incorrect")

	oldCookies := b.c.Jar.Cookies(mustURL(t, e.ts.URL))
	resp, _ = change(bootstrapPassword, "a-brand-new-password", "a-brand-new-password")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/audit", resp.Header.Get("Location"))

	resp, _ = b.get("/ui/audit")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the fresh session works and the flag is cleared")

	// The pre-change session was ended.
	stale := e.browser()
	stale.c.Jar.SetCookies(mustURL(t, e.ts.URL), oldCookies)
	resp, _ = stale.get("/ui/")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)

	resp, _ = e.browser().submitLogin("admin", "a-brand-new-password", "")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/", resp.Header.Get("Location"))
}

func TestLogout(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("alice")
	b := e.login("alice")
	resp, _ := b.post("/ui/logout", nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/ui/login", resp.Header.Get("Location"))
	resp, _ = b.get("/ui/")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
}

func TestSessionExpiryRedirectsWithReturnPath(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("alice", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	b := e.login("alice")
	e.clk.Advance(server.DefaultConfig().SessionTTL + time.Minute)

	resp, _ := b.get("/ui/audit?before=10")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	loc := resp.Header.Get("Location")
	assert.Equal(t, "/ui/login?next=%2Fui%2Faudit%3Fbefore%3D10", loc)

	// htmx requests are told to navigate rather than swap in a login form.
	resp, _ = b.do("GET", "/ui/dashboard/stats", nil, map[string]string{
		"HX-Request": "true", "HX-Current-URL": e.ts.URL + "/ui/"})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, "/ui/login", resp.Header.Get("HX-Redirect"))

	// Signing in again lands on the original page.
	resp, body := b.get(loc)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, `name="next" value="/ui/audit?before=10"`)
	resp, _ = b.submitLogin("alice", "alice-password", "/ui/audit?before=10")
	assert.Equal(t, "/ui/audit?before=10", resp.Header.Get("Location"))
}

func TestOpenRedirectRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("alice")
	for _, next := range []string{"https://evil.example/", "//evil.example/ui/", "/\\evil.example", "/ui/../admin",
		"javascript:alert(1)", "/uix", "/ui/login"} {
		resp, _ := e.browser().submitLogin("alice", "alice-password", next)
		assert.Equal(t, "/ui/", resp.Header.Get("Location"), next)
	}
}

func TestSafeNext(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":                        "/ui/",
		"/ui/":                    "/ui/",
		"/ui/jobs/abc":            "/ui/jobs/abc",
		"/ui/queues/x?state=done": "/ui/queues/x?state=done",
		"/ui//evil.example":       "/ui//evil.example", // a local path on this host
		"//evil.example":          "/ui/",
		"https://evil.example":    "/ui/",
		"http:/ui/x":              "/ui/",
		"/ui/%2e%2e/x":            "/ui/", // decodes to /ui/../x
		"/ui/../x":                "/ui/",
		"/ui/\\x":                 "/ui/",
		"/ui/x\r\nSet-Cookie: a":  "/ui/",
		"/ui/logout":              "/ui/",
		"/api/x":                  "/ui/",
	} {
		assert.Equal(t, want, safeNext(in), in)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	require.NoError(t, err)
	return u
}

// TestCSRF: every mutation needs the session's token, by form field or
// header; cross-site posts are refused even with one.
func TestCSRF(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	b := e.login("root")
	form := func() url.Values { return url.Values{"name": {"csrf-test"}} }

	resp, body := b.do("POST", "/ui/queues", form(), nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "missing token")
	assert.Contains(t, body, "permission")
	f := form()
	f.Set("csrf", "wrong")
	resp, _ = b.do("POST", "/ui/queues", f, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "wrong token")
	resp, _ = b.do("POST", "/ui/queues", form(), map[string]string{server.CSRFHeader: "wrong"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "wrong header")
	_, err := e.st.QueueByName(e.ctx, "csrf-test")
	assert.Error(t, err, "nothing was created")

	resp, _ = b.do("POST", "/ui/logout", nil, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "even logout needs the token")

	f = form()
	f.Set("csrf", b.csrf)
	resp, _ = b.do("POST", "/ui/queues", f, map[string]string{"Sec-Fetch-Site": "cross-site"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "cross-site post")

	resp, _ = b.do("POST", "/ui/queues", form(), map[string]string{server.CSRFHeader: b.csrf})
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "header token (htmx)")
	resp, _ = b.post("/ui/queues", url.Values{"name": {"csrf-test-2"}})
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "form token")

	// Mutations are POST only.
	resp, _ = b.get("/ui/users/root/disable")
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)

	denied := e.logs.events(t, obs.EventAuthDenied)
	var csrfDenials int
	for _, d := range denied {
		if strings.Contains(d[obs.FieldReason].(string), "CSRF") {
			csrfDenials++
			assert.Equal(t, "user:root", d[obs.FieldPrincipal])
		}
	}
	assert.Equal(t, 4, csrfDenials)
}
