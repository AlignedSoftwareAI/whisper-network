package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/obs/obstest"
)

func assertSecurityHeaders(t *testing.T, resp *http.Response, what string) {
	t.Helper()
	h := resp.Header
	csp := h.Get("Content-Security-Policy")
	for _, d := range []string{"default-src 'self'", "script-src 'self'", "style-src 'self'",
		"frame-ancestors 'none'", "base-uri 'none'", "object-src 'none'", "form-action 'self'"} {
		assert.Contains(t, csp, d, what)
	}
	assert.NotContains(t, csp, "unsafe-inline", what)
	assert.NotContains(t, csp, "unsafe-eval", what)
	assert.Equal(t, "DENY", h.Get("X-Frame-Options"), what)
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"), what)
	assert.Equal(t, "same-origin", h.Get("Referrer-Policy"), what)
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	anon := e.browser()
	resp, _ := anon.get("/ui/login")
	assertSecurityHeaders(t, resp, "login")
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	resp, _ = anon.get("/ui/")
	assertSecurityHeaders(t, resp, "redirect to login")
	resp, _ = anon.get("/ui/static/htmx.min.js")
	assertSecurityHeaders(t, resp, "static")
	resp, _ = anon.get("/ui/nope")
	assertSecurityHeaders(t, resp, "404")

	b := e.login("root")
	for _, p := range []string{"/ui/", "/ui/workers", "/ui/users", "/ui/audit", "/ui/jobs/missing"} {
		resp, body := b.get(p)
		assertSecurityHeaders(t, resp, p)
		assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"), p)
		assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"), p)
		// No inline script or style anywhere: the CSP would block it.
		assert.NotContains(t, body, "<style", p)
		assert.NotContains(t, body, "style=", p)
		assert.NotContains(t, strings.NewReplacer(`<script src="/ui/static/htmx.min.js?v=`, "", `<script src="/ui/static/ui.js?v=`, "").Replace(body), "<script", p)
		assert.NotContains(t, body, "hx-on", p)
		assert.Contains(t, body, `"allowEval":false`, p)
	}
}

func TestStaticAssets(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	b := e.browser()

	resp, body := b.get("/ui/static/htmx.min.js")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/javascript; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, "public, max-age=31536000, immutable", resp.Header.Get("Cache-Control"))
	sum := sha256.Sum256([]byte(body))
	assert.Equal(t, htmxSHA256, hex.EncodeToString(sum[:]), "vendored htmx changed; update htmxSHA256 and doc.go")
	assert.Contains(t, body, `version:"`+htmxVersion+`"`)
	etag := resp.Header.Get("ETag")
	require.NotEmpty(t, etag)

	resp, _ = b.do("GET", "/ui/static/htmx.min.js", nil, map[string]string{"If-None-Match": etag})
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)

	resp, body = b.get("/ui/static/style.css")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/css; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Contains(t, body, "prefers-color-scheme: dark")

	resp, _ = b.get("/ui/static/nope.js")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp, _ = b.get("/ui/static/..%2fdoc.go")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// Pages reference assets with a content-hash cache buster.
	_, body = b.get("/ui/login")
	assert.Contains(t, body, `/ui/static/htmx.min.js?v=`+htmxSHA256[:12])
}

// TestHTMXErrorsSwapIntoFlash: a refused htmx action shows its message in
// the page's flash area rather than replacing the target.
func TestHTMXErrorsSwapIntoFlash(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.user("root", api.Grant{Role: api.RoleAdmin, Scope: "*"})
	wid, _ := e.worker("box", "")
	b := e.login("root")
	resp, body := b.do("POST", "/ui/workers/"+wid+"/resume", url.Values{"csrf": {b.csrf}},
		map[string]string{"HX-Request": "true"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "#flash", resp.Header.Get("HX-Retarget"))
	assert.Equal(t, `<p class="error">a pending worker is approved, not resumed</p>`, strings.TrimSpace(body))
}

// TestLogHygiene runs a session of UI actions that handle secrets and
// checks the logs against the never-log rules (DESIGN.md §10.1).
func TestLogHygiene(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	wid, _ := e.worker("box", "garage")

	// First login with a forced change, a failed login, and secrets minted.
	b := e.browser()
	resp, _ := b.submitLogin("admin", "a-wrong-password-attempt", "")
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp, _ = b.submitLogin("admin", bootstrapPassword, "")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	_, body := b.get("/ui/password")
	b.csrf = csrfRE.FindStringSubmatch(body)[1]
	resp, _ = b.post("/ui/password", url.Values{"current": {bootstrapPassword}, "password": {"the-new-admin-password"},
		"confirm": {"the-new-admin-password"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	b.refreshCSRF()

	resp, body = b.post("/ui/clients", url.Values{"name": {"svc"}, "grant": {"submit queue:public"}})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	token := tokenRE.FindString(body)
	require.NotEmpty(t, token)
	resp, body = b.post("/ui/users", url.Values{"username": {"carol"}})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	m := secretRE.FindStringSubmatch(body)
	require.NotNil(t, m)
	generated := m[1]
	resp, _ = b.post("/ui/workers/"+wid+"/approve", url.Values{"queue": {"public"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp, _ = b.post("/ui/queues/public", url.Values{"weight": {"250"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp, _ = b.do("POST", "/ui/queues", url.Values{"name": {"x"}}, nil) // CSRF denial
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp, _ = b.post("/ui/logout", nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	logs := e.logs.Bytes()
	obstest.AssertClean(t, logs)
	for _, secret := range []string{token, generated, bootstrapPassword, "the-new-admin-password",
		"a-wrong-password-attempt", b.csrf} {
		assert.NotContains(t, string(logs), secret)
	}
	assert.NotEmpty(t, e.logs.events(t, "admin.change"))
	assert.NotEmpty(t, e.logs.events(t, "auth.denied"))
}
