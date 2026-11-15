package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"testing"
	"time"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/store"
)

// multipartBody builds an upload body and its content type.
func multipartBody(t *testing.T, params api.JobParams, mediaBytes []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	ph := textproto.MIMEHeader{}
	ph.Set("Content-Disposition", `form-data; name="params"`)
	pw, err := mw.CreatePart(ph)
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(pw).Encode(params))
	fw, err := mw.CreateFormFile("media", "input.bin")
	require.NoError(t, err)
	_, err = fw.Write(mediaBytes)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return buf.Bytes(), mw.FormDataContentType()
}

// heldUpload starts a chunked (unknown-length) upload, sends the first part
// of its body and leaves it in progress. finish sends the rest and returns
// the status.
func (h *harness) heldUpload(token string, body []byte, ctype string) (finish func() int) {
	h.t.Helper()
	pr, pw := io.Pipe()
	r, err := http.NewRequestWithContext(h.ctx, "POST", h.ts.URL+"/queue/public", pr)
	require.NoError(h.t, err)
	r.ContentLength = -1
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", ctype)
	status := make(chan int, 1)
	go func() {
		resp, err := h.ts.Client().Do(r)
		if err != nil {
			status <- -1
			return
		}
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	_, err = pw.Write(body[:len(body)/2])
	require.NoError(h.t, err)
	require.Eventually(h.t, func() bool {
		h.srv.uploading.mu.Lock()
		defer h.srv.uploading.mu.Unlock()
		return len(h.srv.uploading.n) > 0
	}, 5*time.Second, 5*time.Millisecond)
	return func() int {
		_, err := pw.Write(body[len(body)/2:])
		require.NoError(h.t, err)
		require.NoError(h.t, pw.Close())
		return <-status
	}
}

// --- R2: uploads cannot reserve the disk they have not been sent ---

// TestChunkedUploadReservesIncrementally reproduces the original issue: a
// chunked upload reserved its full size cap (2 GiB + 1 MiB) up front, so
// with less free space than that above MinFreeBytes every upload got 507,
// and one in-progress upload blocked all others. Reservations now grow with
// the bytes received.
func TestChunkedUploadReservesIncrementally(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.free.Store(h.srv.cfg.MinFreeBytes + 8<<20) // far below the 2 GiB cap
	slow := h.client(submitOn("public"))
	other := h.client(submitOn("public"))
	body, ctype := multipartBody(t, api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))

	finish := h.heldUpload(slow, body, ctype)
	h.srv.disk.mu.Lock()
	reserved := h.srv.disk.reserved
	h.srv.disk.mu.Unlock()
	assert.LessOrEqual(t, reserved, int64(reserveStep), "an upload in progress holds at most one step")

	h.submit(other, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	assert.Equal(t, http.StatusCreated, finish())
	h.srv.disk.mu.Lock()
	assert.Zero(t, h.srv.disk.reserved, "reservations end with their uploads")
	h.srv.disk.mu.Unlock()
}

// TestUploadsPerPrincipal: one caller may have only UploadsPerPrincipal
// uploads in progress; more get 429 with Retry-After, while other callers
// are unaffected.
func TestUploadsPerPrincipal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) { c.UploadsPerPrincipal = 1 })
	hog := h.client(submitOn("public"))
	other := h.client(submitOn("public"))
	body, ctype := multipartBody(t, api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))

	finish := h.heldUpload(hog, body, ctype)
	resp, b := h.upload(hog, "public", api.JobParams{Model: "small.en"}, []byte("opaque video"), "")
	h.expectProblem(resp, b, http.StatusTooManyRequests, api.ProblemRateLimited)
	assert.Equal(t, "30", resp.Header.Get("Retry-After"))
	h.submit(other, "public", api.JobParams{Model: "small.en"}, []byte("opaque video"))

	assert.Equal(t, http.StatusCreated, finish())
	h.submit(hog, "public", api.JobParams{Model: "small.en"}, []byte("opaque video"))
}

// TestSustainedRate: a body must average MinTransferRate over every sliding
// window. A trickle that beats the stall timeout (one byte just under every
// ReadTimeout) used to hold an upload for hours; now it fails after one
// window.
func TestSustainedRate(t *testing.T) {
	t.Parallel()
	start := time.Unix(1_000_000, 0)
	newBody := func() *deadlineBody {
		return &deadlineBody{rate: 1000, window: 80 * time.Second, marks: []rateMark{{at: start}}}
	}

	trickle := newBody()
	failed := time.Duration(0)
	for at := time.Duration(0); at < time.Hour; at += 59 * time.Second {
		if trickle.tooSlow(1, start.Add(at)) {
			failed = at
			break
		}
	}
	require.NotZero(t, failed, "a trickle is cut off")
	assert.LessOrEqual(t, failed, 2*time.Minute)

	steady := newBody()
	for at := time.Second; at < time.Hour; at += time.Second {
		require.False(t, steady.tooSlow(1100, start.Add(at)), "steady at 1.1× the rate, t=%v", at)
	}

	// A stall shorter than the window, made up afterwards, is fine.
	bursty := newBody()
	for at := time.Second; at < 10*time.Minute; at += time.Second {
		n := 2000
		if at%(80*time.Second) < 30*time.Second {
			n = 0
		}
		require.False(t, bursty.tooSlow(n, start.Add(at)), "bursty, t=%v", at)
	}
	assert.LessOrEqual(t, len(bursty.marks), rateMarks+2, "checkpoints stay bounded")
}

// --- R3: one principal cannot occupy every analysis slot ---

// constantFLAC is canonical FLAC of digital silence in constant frames:
// a dozen bytes per 4096 samples. Any extra metadata blocks follow
// STREAMINFO.
func constantFLAC(t *testing.T, samples int64, blocks ...*meta.Block) []byte {
	t.Helper()
	const block = 4096
	var m memFile
	enc, err := flac.NewEncoder(&m, &meta.StreamInfo{BlockSizeMin: block, BlockSizeMax: block,
		SampleRate: media.SampleRate, NChannels: 1, BitsPerSample: 16, NSamples: uint64(samples)}, blocks...)
	require.NoError(t, err)
	buf := make([]int32, block)
	sub := &frame.Subframe{SubHeader: frame.SubHeader{Pred: frame.PredConstant}}
	f := &frame.Frame{Header: frame.Header{HasFixedBlockSize: true, SampleRate: media.SampleRate,
		Channels: frame.ChannelsMono, BitsPerSample: 16}, Subframes: []*frame.Subframe{sub}}
	for rem := samples; rem > 0; {
		n := int(min(rem, block))
		rem -= int64(n)
		f.BlockSize = uint16(n)
		sub.Samples, sub.NSamples = buf[:n], n
		require.NoError(t, enc.WriteFrame(f))
	}
	require.NoError(t, enc.Close())
	return m.buf
}

// TestAnalysisPerPrincipal: while a principal holds its analysis slot its
// next fast-path upload waits BusyWait and gets 429; other principals still
// get the remaining CPUs.
func TestAnalysisPerPrincipal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) {
		c.AnalysisSlots = 4
		c.BusyWait = 50 * time.Millisecond
	})
	hog := h.client(submitOn("public"))
	other := h.client(submitOn("public"))
	_, hash, err := auth.ParseToken(hog)
	require.NoError(t, err)
	c, err := h.st.ClientByTokenHash(h.ctx, hash)
	require.NoError(t, err)
	release, err := h.srv.analysisSlot(h.ctx, c.Principal())
	require.NoError(t, err)

	resp, b := h.upload(hog, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)), "")
	h.expectProblem(resp, b, http.StatusTooManyRequests, api.ProblemRateLimited)
	assert.Equal(t, "30", resp.Header.Get("Retry-After"))
	h.submit(other, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	h.submit(hog, "public", api.JobParams{Model: "small.en"}, []byte("opaque video")) // no analysis needed
	release()
	h.submit(hog, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	h.srv.analysisBy.mu.Lock()
	assert.Empty(t, h.srv.analysisBy.sems, "idle per-principal semaphores are dropped")
	h.srv.analysisBy.mu.Unlock()
}

// TestImplausiblyCompressedFLACGoesToPrepare: a fast-path FLAC averaging
// more than fastPathSamplesPerByte samples per frame byte is not decoded on
// the server, however much metadata pads it; it becomes a prepare task for
// a sandboxed worker.
func TestImplausiblyCompressedFLACGoesToPrepare(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tok := h.client(submitOn("public"))
	j := h.submit(tok, "public", api.JobParams{Model: "small.en"}, constantFLAC(t, 3*60*media.SampleRate))
	assert.Equal(t, api.JobPreparing, j.State)
	assert.Equal(t, 1, j.Tasks.Total, "one prepare task")

	// The same frames behind a PADDING block that brings the whole file
	// under the limit: metadata is not counted, so still a prepare task.
	const samples = 3 * 60 * media.SampleRate
	pad := &meta.Block{Header: meta.Header{Type: meta.TypePadding, Length: samples / (fastPathSamplesPerByte - 2)}}
	padded := constantFLAC(t, samples, pad)
	require.Less(t, float64(samples)/float64(len(padded)), float64(fastPathSamplesPerByte), "fixture")
	j = h.submit(tok, "public", api.JobParams{Model: "small.en"}, padded)
	assert.Equal(t, api.JobPreparing, j.State, "padding does not buy the fast path")

	j = h.submit(tok, "public", api.JobParams{Model: "small.en"}, canonicalFLAC(t, speech(2, media.SampleRate)))
	assert.Equal(t, api.JobQueued, j.State, "ordinary FLAC still takes the fast path")
}

// --- R4: revoking process ends the worker's leases on that queue ---

// TestRevokeProcessCancelsLease reproduces the original issue: after its
// process grant on a queue was revoked a worker's live lease kept
// heartbeating (and could go on receiving the queue's media) until the
// task ended. Now the revocation cancels the lease in the same
// transaction, the task requeues without using an attempt, and the
// heartbeat says 410.
func TestRevokeProcessCancelsLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tok := h.client(submitOn("public"))
	j := h.submit(tok, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	l := h.claim(wid, wtok, "", nil)
	require.NotNil(t, l)

	resp, b := h.call("DELETE", "/admin/grants?principal=worker:"+wid+"&role=process&scope=queue:public", h.admin, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(b))

	resp, b = h.call("POST", "/lease/"+l.ID+"/heartbeat", wtok, nil)
	h.expectProblem(resp, b, http.StatusGone, api.ProblemLeaseGone)
	resp, b = h.call("GET", "/lease/"+l.ID+"/input", wtok, nil)
	h.expectProblem(resp, b, http.StatusGone, api.ProblemLeaseGone)
	got := h.job(j.ID)
	assert.Equal(t, 1, got.Tasks.Pending, "the task is pending again")

	other, otok := h.activeWorker(api.WorkerFacts{Hostname: "other"}, "public")
	l2 := h.claim(other, otok, "", nil)
	require.NotNil(t, l2)
	sl, err := h.st.Lease(h.ctx, l2.ID, other)
	require.NoError(t, err)
	assert.Equal(t, 1, sl.Attempt, "the cancelled lease's attempt was refunded")
	assert.Nil(t, h.claim(wid, wtok, "", nil), "the revoked worker gets nothing more")
}

// TestHeartbeatMaxLeaseAge: heartbeats cannot keep a lease alive past
// max(MaxLeaseAge, LeaseAgeFactor × task cost); then the lease expires.
func TestHeartbeatMaxLeaseAge(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) {
		c.MaxLeaseAge = time.Hour
		c.LeaseAgeFactor = 4
	})
	tok := h.client(submitOn("public"))
	h.submit(tok, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	l := h.claim(wid, wtok, "", nil)
	require.NotNil(t, l)
	for elapsed := time.Duration(0); elapsed < time.Hour-time.Minute; elapsed += time.Minute {
		h.clk.Advance(time.Minute)
		resp, b := h.call("POST", "/lease/"+l.ID+"/heartbeat", wtok, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%v: %s", elapsed, b)
	}
	h.clk.Advance(time.Minute)
	resp, b := h.call("POST", "/lease/"+l.ID+"/heartbeat", wtok, nil)
	h.expectProblem(resp, b, http.StatusGone, api.ProblemLeaseGone)
}

// --- R5: one source cannot fill the pending-worker cap ---

func TestPendingWorkersPerSource(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) {
		c.MaxPendingPerSource = 2
		c.RegisterBurst = 100
	})
	first, _ := h.worker(api.WorkerFacts{Hostname: "a"})
	h.worker(api.WorkerFacts{Hostname: "b"})
	resp, b := h.call("POST", "/worker", "", api.WorkerFacts{Hostname: "c", SandboxLevel: api.SandboxFull})
	h.expectProblem(resp, b, http.StatusTooManyRequests, api.ProblemRateLimited)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))

	h.expect("POST", "/admin/workers/"+first+"/approve", h.admin, api.ApproveWorkerRequest{}, http.StatusNoContent, nil)
	h.worker(api.WorkerFacts{Hostname: "c"}) // approving one frees its source's slot
}

// --- R6: a forced password change gates the API too ---

// sessionCookie logs user in through the UI seam and returns the cookie.
func (h *harness) sessionCookie(username, password string) (*http.Cookie, *store.Session) {
	h.t.Helper()
	u, err := h.srv.Login(h.ctx, username, password)
	require.NoError(h.t, err)
	rec := httptest.NewRecorder()
	sess, err := h.srv.StartSession(rec, httptest.NewRequest("POST", "/ui/login", nil), u)
	require.NoError(h.t, err)
	return rec.Result().Cookies()[0], sess
}

// TestMustChangePasswordBlocksAPI reproduces the original issue: a user
// whose password was set by an administrator could skip the forced change
// by calling the API with their session. Every API route now refuses such
// a session with 403 password-change-required.
func TestMustChangePasswordBlocksAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, err := h.srv.Ops().CreateUser(h.ctx, CLIActor(), api.CreateUserRequest{Username: "dora",
		Password: "temporary-password", Grants: []api.Grant{submitOn("public")}})
	require.NoError(t, err)
	generated, err := h.srv.Ops().ResetPassword(h.ctx, CLIActor(), "dora", "") // must change
	require.NoError(t, err)

	cookie, _ := h.sessionCookie("dora", generated)
	get := func() (*http.Response, []byte) {
		resp := h.do(req{method: "GET", path: "/queue/public", header: map[string]string{"Cookie": cookie.String()}})
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp, b
	}
	resp, b := get()
	h.expectProblem(resp, b, http.StatusForbidden, ProblemPasswordChangeRequired)

	u, err := h.st.UserByUsername(h.ctx, "dora")
	require.NoError(t, err)
	require.NoError(t, h.srv.ChangePassword(h.ctx, u, u.PasswordHash, "dora-own-password"))
	cookie, _ = h.sessionCookie("dora", "dora-own-password")
	resp, b = get()
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(b))
}

// --- R7: a password reset between verification and session creation wins ---

func TestLoginRacesPasswordReset(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	db, err := sql.Open("sqlite", filepath.Join(h.dir, "wn.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	// Each case has its own user, so the disable case changes nothing but
	// disabled_at: it isolates the disabled check from the hash check.
	for _, tc := range []struct {
		name       string
		interleave func(user string)
	}{
		{"reset", func(user string) {
			_, err := h.srv.Ops().ResetPassword(h.ctx, CLIActor(), user, "")
			require.NoError(t, err)
		}},
		{"disable", func(user string) {
			require.NoError(t, h.srv.Ops().DisableUser(h.ctx, CLIActor(), user))
		}},
	} {
		user := "eve-" + tc.name
		_, err := h.srv.Ops().CreateUser(h.ctx, CLIActor(), api.CreateUserRequest{Username: user, Password: "old-password-1"})
		require.NoError(t, err)
		u, err := h.srv.Login(h.ctx, user, "old-password-1")
		require.NoError(t, err, tc.name)
		tc.interleave(user) // lands after verification, before the session exists
		if tc.name == "disable" {
			now, err := h.st.UserByUsername(h.ctx, user)
			require.NoError(t, err)
			require.Equal(t, u.PasswordHash, now.PasswordHash, "fixture: only disabled_at changed")
		}
		rec := httptest.NewRecorder()
		_, err = h.srv.StartSession(rec, httptest.NewRequest("POST", "/ui/login", nil), u)
		require.ErrorIs(t, err, ErrBadCredentials, tc.name)
		assert.Empty(t, rec.Result().Cookies(), tc.name)
		var n int
		require.NoError(t, db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM sessions s JOIN users u
			ON u.id = s.user_id WHERE u.username = ?`, user).Scan(&n))
		assert.Zero(t, n, tc.name)
	}
}

// TestChangePasswordRacesPasswordReset: a self-service change checks the
// current password, then sets the new one. An administrator's reset (or a
// disable) that lands between the two used to be overwritten, leaving the
// attacker holding the account; the change now confirms, in its own
// transaction, the hash it checked, and is refused otherwise.
func TestChangePasswordRacesPasswordReset(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, tc := range []struct {
		name       string
		interleave func(user string)
		after      func(user, checkedHash string) // the administrator's outcome must hold
	}{
		{"reset", func(user string) {
			_, err := h.srv.Ops().ResetPassword(h.ctx, CLIActor(), user, "admin-chosen-password")
			require.NoError(t, err)
		}, func(user, _ string) {
			_, err := h.srv.Login(h.ctx, user, "admin-chosen-password")
			require.NoError(t, err, "the administrator's password stays valid")
		}},
		{"disable", func(user string) {
			require.NoError(t, h.srv.Ops().DisableUser(h.ctx, CLIActor(), user))
		}, func(user, checkedHash string) {
			u, err := h.st.UserByUsername(h.ctx, user)
			require.NoError(t, err)
			require.NotNil(t, u.DisabledAt, "still disabled")
			require.Equal(t, checkedHash, u.PasswordHash, "password unchanged")
		}},
	} {
		user := "mallory-" + tc.name
		_, err := h.srv.Ops().CreateUser(h.ctx, CLIActor(), api.CreateUserRequest{Username: user, Password: "stolen-password-1"})
		require.NoError(t, err)
		checked, err := h.srv.Login(h.ctx, user, "stolen-password-1") // the current-password check
		require.NoError(t, err, tc.name)
		tc.interleave(user)
		err = h.srv.ChangePassword(h.ctx, checked, checked.PasswordHash, "attacker-password-2")
		require.ErrorIs(t, err, ErrBadCredentials, tc.name)
		tc.after(user, checked.PasswordHash)
		_, err = h.srv.Login(h.ctx, user, "attacker-password-2")
		require.ErrorIs(t, err, ErrBadCredentials, "%s: the attacker's password was not set", tc.name)
	}
}
