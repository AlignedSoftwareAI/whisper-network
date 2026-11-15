package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/obs/obstest"
	"github.com/deweysasser/whisper-network/internal/store"
)

// TestUISeamHelpers covers the helpers the web UI uses beyond the session
// seam: ClientIP, Now, ErrorStatus, DeleteJob and Ops.DisableUser.
func TestUISeamHelpers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) { c.TrustProxy = true })

	r := httptest.NewRequest("GET", "/ui/", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.7")
	assert.Equal(t, "198.51.100.7", h.srv.ClientIP(r), "same rightmost-XFF rule as the API")
	assert.Equal(t, h.clk.Now(), h.srv.Now())

	for name, tc := range map[string]struct {
		err    error
		status int
		detail string
	}{
		"not found": {store.ErrNotFound, http.StatusNotFound, "not found"},
		"problem":   {badRequest("bad thing"), http.StatusBadRequest, "bad thing"},
		"internal":  {errors.New("disk on fire at /secret/path"), http.StatusInternalServerError, "internal error"},
	} {
		st, d := ErrorStatus(tc.err)
		assert.Equal(t, tc.status, st, name)
		assert.Equal(t, tc.detail, d, name)
	}

	// DeleteJob removes the job and audits it like DELETE /job/{id}.
	tok := h.client(submitOn("public"))
	j := h.submit(tok, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, 16000)))
	admin, err := h.srv.Login(h.ctx, "admin", "admin-password")
	require.NoError(t, err)
	p, err := h.srv.PrincipalForUser(h.ctx, admin)
	require.NoError(t, err)
	require.NoError(t, h.srv.DeleteJob(h.ctx, p, j.ID))
	_, err = h.st.Job(h.ctx, j.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Len(t, h.logs.events(t, obs.EventJobDeleted), 1)
	audit, err := h.st.ListAudit(h.ctx, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, "job.delete", audit[0].Action)
	assert.Equal(t, "user:admin", audit[0].Actor)

	// DisableUser blocks login and kills sessions, and is audited.
	_, err = h.srv.Ops().CreateUser(h.ctx, CLIActor(), api.CreateUserRequest{Username: "carol", Password: "carol-password"})
	require.NoError(t, err)
	carol, err := h.srv.Login(h.ctx, "carol", "carol-password")
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	_, err = h.srv.StartSession(rec, httptest.NewRequest("POST", "/ui/login", nil), carol)
	require.NoError(t, err)
	require.NoError(t, h.srv.Ops().DisableUser(h.ctx, p.Actor(), "carol"))
	_, err = h.srv.Login(h.ctx, "carol", "carol-password")
	assert.ErrorIs(t, err, ErrBadCredentials)
	r2 := httptest.NewRequest("GET", "/ui/", nil)
	r2.AddCookie(rec.Result().Cookies()[0])
	sess, _, err := h.srv.SessionFromRequest(r2)
	require.NoError(t, err)
	assert.Nil(t, sess, "sessions end on disable")
	audit, err = h.st.ListAudit(h.ctx, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, "user.disable", audit[0].Action)
	assert.ErrorIs(t, h.srv.Ops().DisableUser(h.ctx, p.Actor(), "nobody"), store.ErrNotFound)
	obstest.AssertClean(t, h.logs.Bytes())
}
