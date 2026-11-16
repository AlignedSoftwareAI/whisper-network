package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/blobs"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/models"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/store"
)

// --- fix: pending-worker squatting ---

// TestPendingWorkerPollingIsNotRecorded: a pending worker polling its own
// state writes nothing, so it cannot keep itself fresh; an approved one is
// recorded as seen.
func TestPendingWorkerPollingIsNotRecorded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id, tok := h.worker(api.WorkerFacts{})
	h.expect("GET", "/worker/"+id, tok, nil, http.StatusOK, nil)
	wk, err := h.st.WorkerByID(h.ctx, id)
	require.NoError(t, err)
	assert.Nil(t, wk.LastSeenAt, "pending polls are not recorded")

	h.expect("POST", "/admin/workers/"+id+"/approve", h.admin, api.ApproveWorkerRequest{Queues: []string{"public"}},
		http.StatusNoContent, nil)
	h.expect("GET", "/worker/"+id, tok, nil, http.StatusOK, nil)
	wk, err = h.st.WorkerByID(h.ctx, id)
	require.NoError(t, err)
	assert.NotNil(t, wk.LastSeenAt)
}

// TestPendingWorkerExpiresDespitePolling: expiry counts from registration.
func TestPendingWorkerExpiresDespitePolling(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id, tok := h.worker(api.WorkerFacts{})
	for range 8 {
		h.clk.Advance(24 * time.Hour)
		h.call("GET", "/worker/"+id, tok, nil)
		h.srv.reapOnce(h.ctx)
	}
	_, err := h.st.WorkerByID(h.ctx, id)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestRegistrationKeysIPv6By64: rotating addresses within one /64 does not
// buy more registrations.
func TestRegistrationKeysIPv6By64(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) {
		c.RegisterBurst = 1
		c.TrustProxy = true
	})
	facts := api.WorkerFacts{Hostname: "h", SandboxLevel: api.SandboxFull}
	from := func(ip string) int {
		r := h.do(req{method: "POST", path: "/worker", body: jsonBody(t, facts),
			header: map[string]string{"X-Forwarded-For": ip}})
		return r.StatusCode
	}
	assert.Equal(t, http.StatusCreated, from("2001:db8:1:2::1"))
	assert.Equal(t, http.StatusTooManyRequests, from("2001:db8:1:2:ffff::7"), "same /64")
	assert.Equal(t, http.StatusCreated, from("2001:db8:1:3::1"), "another /64")
}

func TestReaperPrunesRejectedWorkers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id, tok := h.worker(api.WorkerFacts{})
	_, err := h.st.SetWorkerState(h.ctx, id, api.WorkerRejected, "user:1")
	require.NoError(t, err)

	h.clk.Advance(h.srv.cfg.RejectedWorkerRetention - time.Second)
	h.srv.reapOnce(h.ctx)
	resp, b := h.call("GET", "/worker/"+id, tok, nil)
	h.expectProblem(resp, b, http.StatusGone, api.ProblemWorkerRejected)

	h.clk.Advance(2 * time.Second)
	h.srv.reapOnce(h.ctx)
	_, err = h.st.WorkerByID(h.ctx, id)
	assert.ErrorIs(t, err, store.ErrNotFound)
	resp, _ = h.call("GET", "/worker/"+id, tok, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// --- fix: password hashing memory ---

func TestPasswordSlots(t *testing.T) {
	t.Parallel()
	def := auth.DefaultPasswordParams()
	assert.Equal(t, 2, passwordSlots(1, def), "never fewer than two")
	perHash := int64(def.MemoryKiB) << 10
	assert.Equal(t, max(2, min(runtime.GOMAXPROCS(0), 3)), passwordSlots(3*perHash, def))
	small := auth.PasswordParams{MemoryKiB: 64, Time: 1, Threads: 1}
	assert.Equal(t, passwordSlots(3*perHash, def), passwordSlots(3*perHash, small),
		"sized for the default-cost dummy hash too")
}

// TestLoginWaitsForPasswordSlot: with every slot busy, a login waits
// BusyWait and is then refused with 503; a cancelled caller is not a fault.
func TestLoginWaitsForPasswordSlot(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) {
		c.BusyWait = 50 * time.Millisecond
		c.PasswordMemoryBudget = 1 // two slots
	})
	r1, err := h.srv.PasswordSlot(h.ctx)
	require.NoError(t, err)
	r2, err := h.srv.PasswordSlot(h.ctx)
	require.NoError(t, err)

	_, err = h.srv.Login(h.ctx, "admin", "admin-password")
	status, _ := ErrorStatus(err)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, 5, asProblem(err).retryAfter)
	err = h.srv.ChangePassword(h.ctx, &store.User{ID: 1}, "", "irrelevant-password")
	status, _ = ErrorStatus(err)
	assert.Equal(t, http.StatusServiceUnavailable, status)

	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	_, err = h.srv.Login(ctx, "admin", "admin-password")
	assert.ErrorIs(t, err, errClientGone)

	r1()
	u, err := h.srv.Login(h.ctx, "admin", "admin-password")
	require.NoError(t, err)
	assert.Equal(t, "admin", u.Username)
	r2()
}

// --- fix: slowloris and per-request deadlines ---

// serve runs a second server over h's data on a real listener (Serve, so
// the http.Server timeouts apply) and returns its base URL.
func serve(t *testing.T, h *harness) string {
	t.Helper()
	srv, err := New(h.srv.cfg, Deps{Store: h.st, Blobs: h.blobs, Logger: h.srv.log, Metrics: h.metrics,
		Now: h.clk.Now, FreeBytes: func(string) (int64, error) { return h.free.Load(), nil },
		LookupModel: lookupTestModel})
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln, nil) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	url := "http://" + ln.Addr().String()
	require.Eventually(t, func() bool {
		resp, err := http.Get(url + "/healthz")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)
	return url
}

func fastTimeouts(c *Config, _ *store.Config) {
	c.ReadTimeout = 300 * time.Millisecond
}

// TestSlowBodyIsCutOff: a client that stops sending its body is
// disconnected after ReadTimeout.
func TestSlowBodyIsCutOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fastTimeouts)
	url := serve(t, h)
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	require.NoError(t, err)
	defer conn.Close()
	start := time.Now()
	_, err = io.WriteString(conn, "POST /worker HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n"+
		"Content-Length: 100\r\n\r\n{")
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = io.ReadAll(conn)
	require.NoError(t, err, "the server closes the connection")
	assert.Less(t, time.Since(start), 3*time.Second)
}

// trickle writes b in n pieces, pausing between them.
func trickle(w *io.PipeWriter, b []byte, n int, pause time.Duration) {
	step := (len(b) + n - 1) / n
	for len(b) > 0 {
		k := min(step, len(b))
		if _, err := w.Write(b[:k]); err != nil {
			return
		}
		b = b[k:]
		time.Sleep(pause)
	}
	_ = w.Close()
}

func multipartUpload(t *testing.T, params api.JobParams, mediaBytes []byte) ([]byte, string) {
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

// TestStreamingUploadOutlivesReadTimeout: an upload that keeps making
// progress may take longer than ReadTimeout; one that stalls may not.
func TestStreamingUploadOutlivesReadTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fastTimeouts)
	url := serve(t, h)
	client := h.client(submitOn("public"))
	body, ctype := multipartUpload(t, api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))

	send := func(pause time.Duration, pieces int) (*http.Response, error) {
		pr, pw := io.Pipe()
		go trickle(pw, body, pieces, pause)
		r, err := http.NewRequest("POST", url+"/queue/public", pr)
		require.NoError(t, err)
		r.ContentLength = int64(len(body))
		r.Header.Set("Authorization", "Bearer "+client)
		r.Header.Set("Content-Type", ctype)
		return http.DefaultClient.Do(r)
	}
	start := time.Now()
	resp, err := send(100*time.Millisecond, 10)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Greater(t, time.Since(start), 3*h.srv.cfg.ReadTimeout, "the upload outlived ReadTimeout")

	resp, err = send(time.Second, 2) // stalls longer than ReadTimeout
	if err == nil {
		resp.Body.Close()
		assert.NotEqual(t, http.StatusCreated, resp.StatusCode)
	}
}

// TestLongRequestsOutliveReadTimeout: a long-poll (with and without a
// body) is not cut off by ReadTimeout.
func TestLongRequestsOutliveReadTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fastTimeouts)
	url := serve(t, h)
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	for _, body := range []string{"", `{"kinds":["transcribe"],"models":["small.en"]}`} {
		r, err := http.NewRequest("POST", url+"/worker/"+wid+"/leases?wait=1s", strings.NewReader(body))
		require.NoError(t, err)
		r.Header.Set("Authorization", "Bearer "+wtok)
		start := time.Now()
		resp, err := http.DefaultClient.Do(r)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusNoContent, resp.StatusCode, "body %q", body)
		assert.GreaterOrEqual(t, time.Since(start), 900*time.Millisecond, "waited the full long-poll")
	}
}

func TestTransferTime(t *testing.T) {
	t.Parallel()
	s := &Server{cfg: Config{ReadTimeout: time.Minute, MinTransferRate: 1 << 20}}
	assert.Equal(t, time.Minute, s.transferTime(0))
	assert.Equal(t, time.Minute+10*time.Second, s.transferTime(10<<20))
}

// --- fix: analysis CPU ---

// TestAnalysisSlotBusy: with every analysis slot taken a fast-path upload
// waits BusyWait, then gets 503 with Retry-After and leaves nothing behind;
// uploads that need no analysis are unaffected.
func TestAnalysisSlotBusy(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) {
		c.AnalysisSlots = 1
		c.BusyWait = 50 * time.Millisecond
	})
	client := h.client(submitOn("public"))
	release, err := h.srv.analysisSlot(h.ctx, "client:another")
	require.NoError(t, err)

	resp, b := h.upload(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)), "")
	h.expectProblem(resp, b, http.StatusServiceUnavailable, api.ProblemRateLimited)
	assert.Equal(t, "30", resp.Header.Get("Retry-After"))
	dirs, err := h.blobs.Jobs()
	require.NoError(t, err)
	assert.Empty(t, dirs, "the refused upload is removed")

	h.submit(client, "public", api.JobParams{Model: "small.en"}, []byte("opaque video"))
	release()
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
}

// TestCanonicalizeStopsWhenClientLeaves: a cancelled request stops the
// decode and is reported as the client going away, not as bad media.
func TestCanonicalizeStopsWhenClientLeaves(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	for typ, data := range map[string][]byte{
		media.TypeWAV:  canonicalWAV(speech(2, media.SampleRate)),
		media.TypeFLAC: canonicalFLAC(t, speech(2, media.SampleRate)),
	} {
		id := "cancel-" + strings.ReplaceAll(typ, "/", "-")
		require.NoError(t, h.blobs.WriteFile(id, blobs.Original, data))
		c, err := h.srv.canonicalizeUpload(ctx, "client:test", id, typ)
		assert.Nil(t, c)
		assert.ErrorIs(t, err, errClientGone, typ)
	}
}

func TestCtxReader(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	r := ctxReader{ctx: ctx, r: strings.NewReader("abc")}
	b := make([]byte, 1)
	n, err := r.Read(b)
	assert.Equal(t, 1, n)
	require.NoError(t, err)
	cancel()
	_, err = r.Read(b)
	assert.ErrorIs(t, err, context.Canceled)
}

// --- fix: transient result upload failures ---

func TestInvalidJSON(t *testing.T) {
	t.Parallel()
	decode := func(s string) error {
		var res api.TranscribeResult
		return json.NewDecoder(strings.NewReader(s)).Decode(&res)
	}
	assert.True(t, invalidJSON(decode(`{"segments": [}`)), "syntax")
	assert.True(t, invalidJSON(decode(`"not an object"`)), "type")
	assert.True(t, invalidJSON(decode(``)), "empty body")
	assert.False(t, invalidJSON(decode(`{"segments": [`)), "truncated: a transfer failure")
	assert.False(t, invalidJSON(errors.New("connection reset by peer")))
	assert.False(t, invalidJSON(context.Canceled))
}

// TestTruncatedResultKeepsLease: a result whose upload breaks off does not
// fail the lease or spend an attempt; the worker can retry.
func TestTruncatedResultKeepsLease(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)

	full, err := json.Marshal(result(l, "hello"))
	require.NoError(t, err)
	conn, err := net.Dial("tcp", strings.TrimPrefix(h.ts.URL, "http://"))
	require.NoError(t, err)
	_, err = fmt.Fprintf(conn, "PUT /lease/%s/result HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n"+
		"Content-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", l.ID, wtok, len(full), full[:len(full)/2])
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.Eventually(t, func() bool {
		for _, e := range h.logs.events(t, obs.EventHTTPRequest) {
			if e[obs.FieldRoute] == "/lease/{id}/result" {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)

	assert.Empty(t, h.logs.events(t, obs.EventTaskFailed))
	h.expect("POST", "/lease/"+l.ID+"/heartbeat", wtok, nil, http.StatusOK, nil)
	resp, b := h.putResult(l, wtok, result(l, "hello"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
}

// --- fix: orphan sweep ---

func TestSweepSparesUploadsInProgress(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) { c.OrphanGrace = time.Minute })
	old, now := h.clk.Now().Add(-time.Hour), h.clk.Now()
	mk := func(id string, childTime time.Time) {
		require.NoError(t, h.blobs.WriteFile(id, blobs.Original, []byte("x")))
		require.NoError(t, os.Chtimes(blobPath(t, h, id, blobs.Original), childTime, childTime))
		require.NoError(t, os.Chtimes(blobPath(t, h, id, ""), old, old))
	}
	mk("inflight-job", old) // registered: spared however stale it looks
	mk("growing-job", now)  // a file still being written: spared
	mk("dead-job", old)
	done := h.srv.uploads.add("inflight-job")

	h.srv.sweep(h.ctx)
	exists := func(id string) bool {
		_, err := os.Stat(blobPath(t, h, id, ""))
		return err == nil
	}
	assert.True(t, exists("inflight-job"))
	assert.True(t, exists("growing-job"))
	assert.False(t, exists("dead-job"))

	done()
	h.srv.sweep(h.ctx)
	assert.False(t, exists("inflight-job"), "swept once its upload is over")
}

// --- fix: disk guard ---

// TestConcurrentUploadsReserveDisk: an upload in progress holds one step
// of free space ahead of the media it has written, so a second upload that
// would overshoot MinFreeBytes on top of it stops with 507; the
// reservation ends with the upload.
func TestConcurrentUploadsReserveDisk(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	body, ctype := multipartUpload(t, api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	// Room for the first upload's step and hardly more. (The fake free
	// space does not shrink as uploads write, so the second upload is
	// refused on its first write, not after a few.)
	h.free.Store(h.srv.cfg.MinFreeBytes + reserveStep + 100)
	media0 := bytes.Index(body, []byte("input.bin"))
	media0 += bytes.Index(body[media0:], []byte("\r\n\r\n")) + 4

	pr, pw := io.Pipe()
	r, err := http.NewRequestWithContext(h.ctx, "POST", h.ts.URL+"/queue/public", pr)
	require.NoError(t, err)
	r.ContentLength = int64(len(body))
	r.Header.Set("Authorization", "Bearer "+client)
	r.Header.Set("Content-Type", ctype)
	first := make(chan int, 1)
	go func() {
		resp, err := h.ts.Client().Do(r)
		if err != nil {
			first <- -1
			return
		}
		resp.Body.Close()
		first <- resp.StatusCode
	}()
	_, err = pw.Write(body[:media0+256]) // the params and a little media
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		h.srv.disk.mu.Lock()
		defer h.srv.disk.mu.Unlock()
		return h.srv.disk.reserved > 0
	}, 5*time.Second, 5*time.Millisecond)

	resp, b := h.upload(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)), "")
	h.expectProblem(resp, b, http.StatusInsufficientStorage, api.ProblemStorageFull)

	_, err = pw.Write(body[media0+256:])
	require.NoError(t, err)
	require.NoError(t, pw.Close())
	assert.Equal(t, http.StatusCreated, <-first)
	h.srv.disk.mu.Lock()
	assert.Zero(t, h.srv.disk.reserved, "reservations end with their uploads")
	h.srv.disk.mu.Unlock()
	h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
}

// TestSlowUploadsDoNotStarveOthers reproduces the reservation-holding
// attack: uploads that declare a large Content-Length and then trickle
// used to reserve a 64 MiB step (or their declared size) before their
// bytes arrived, so a couple of them kept other callers at 507 for as long
// as they could stay connected. Reservations now track bytes received
// plus one small step, so with headroom just above a legitimate upload's
// size (plus one step per trickler) the legitimate upload still fits.
func TestSlowUploadsDoNotStarveOthers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	hog := h.client(submitOn("public"))
	honest := h.client(submitOn("public"))
	legit := bytes.Repeat([]byte("opaque video "), (4<<20)/13) // ~4 MiB
	// Headroom: the legitimate upload, one 1 MiB step per trickler, and a
	// little slack. (A literal, not reserveStep: the test pins the step.)
	require.LessOrEqual(t, reserveStep, 1<<20)
	h.free.Store(h.srv.cfg.MinFreeBytes + int64(len(legit)) + 2<<20 + 512<<10)

	// Two tricklers, each declaring 3 MiB and sending 64 KiB of it.
	hogBody, ctype := multipartUpload(t, api.JobParams{Model: "small.en"}, make([]byte, 3<<20))
	var pipes []*io.PipeWriter
	statuses := make(chan int, 2)
	for range 2 {
		pr, pw := io.Pipe()
		pipes = append(pipes, pw)
		r, err := http.NewRequestWithContext(h.ctx, "POST", h.ts.URL+"/queue/public", pr)
		require.NoError(t, err)
		r.ContentLength = int64(len(hogBody))
		r.Header.Set("Authorization", "Bearer "+hog)
		r.Header.Set("Content-Type", ctype)
		go func() {
			resp, err := h.ts.Client().Do(r)
			if err != nil {
				statuses <- -1
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
		_, err = pw.Write(hogBody[:64<<10])
		require.NoError(t, err)
	}
	reserved := func() int64 {
		h.srv.disk.mu.Lock()
		defer h.srv.disk.mu.Unlock()
		return h.srv.disk.reserved
	}
	inflight := func() int {
		h.srv.uploads.mu.Lock()
		defer h.srv.uploads.mu.Unlock()
		return len(h.srv.uploads.ids)
	}
	require.Eventually(t, func() bool { return inflight() == 2 && reserved() >= 2*(1<<20-128<<10) },
		5*time.Second, 5*time.Millisecond, "both tricklers are writing media")
	assert.LessOrEqual(t, reserved(), int64(2<<20), "each trickler holds at most one step beyond what it sent")

	resp, b := h.upload(honest, "public", api.JobParams{Model: "small.en"}, legit, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode, "the legitimate upload fits: %s", b)

	for _, pw := range pipes {
		pw.CloseWithError(errors.New("trickler gives up"))
	}
	for range pipes {
		<-statuses
	}
	require.Eventually(t, func() bool { return reserved() == 0 }, 5*time.Second, 5*time.Millisecond,
		"reservations end with their uploads")
}

// TestUploadDeclaredTooLarge: a Content-Length that could not fit above
// MinFreeBytes is refused before the body is read; nothing is reserved
// from it.
func TestUploadDeclaredTooLarge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	h.free.Store(h.srv.cfg.MinFreeBytes + 2<<20)
	resp, b := h.upload(client, "public", api.JobParams{Model: "small.en"}, bytes.Repeat([]byte("v"), 4<<20), "")
	h.expectProblem(resp, b, http.StatusInsufficientStorage, api.ProblemStorageFull)
	resp, b = h.upload(client, "public", api.JobParams{Model: "small.en"}, bytes.Repeat([]byte("v"), 1<<20), "")
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(b))
}

// TestSlowParamsIs408: a body that falls below MinTransferRate while the
// params part is still arriving is a 408, like one that falls behind in
// the media part, not a malformed request.
func TestSlowParamsIs408(t *testing.T) {
	t.Parallel()
	for name, prefix := range map[string]string{
		"before params": "",
		"inside params": "--B\r\nContent-Disposition: form-data; name=\"params\"\r\n\r\n{\"model\":",
	} {
		mr := multipart.NewReader(io.MultiReader(strings.NewReader(prefix), iotest.ErrReader(errTooSlow)), "B")
		_, err := readParams(mr)
		require.Error(t, err, name)
		assert.Equal(t, http.StatusRequestTimeout, asProblem(err).status, name)
	}
}

// TestUploadStopsWhenDiskRunsLow: free space is re-checked while an upload
// streams, and the write stops once it falls below MinFreeBytes.
func TestUploadStopsWhenDiskRunsLow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res, err := h.srv.reserveDisk(reserveStep)
	require.NoError(t, err)
	defer res.release()
	g := &guardedWriter{w: io.Discard, res: res}
	chunk := make([]byte, 1<<20)
	for range reserveStep / len(chunk) {
		_, err = g.Write(chunk)
		require.NoError(t, err)
	}
	h.free.Store(h.srv.cfg.MinFreeBytes - 1)
	for i := 0; err == nil && i <= reserveStep/len(chunk); i++ {
		_, err = g.Write(chunk)
	}
	assert.ErrorIs(t, err, errDiskLow)
}

// TestQueueUploadCapIsClamped: a queue override cannot raise the cap above
// the server's --max-upload-bytes. The API refuses such a cap (see
// TestQueueCapAboveServerCap); one stored while the server's cap was
// higher is still clamped.
func TestQueueUploadCapIsClamped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) { c.MaxUploadBytes = 4000 })
	client := h.client(submitOn("public"))
	limit := int64(1 << 30)
	pub, err := h.st.QueueByName(h.ctx, "public")
	require.NoError(t, err)
	_, err = h.st.PatchQueue(h.ctx, pub.ID, api.QueuePatch{MaxUploadBytes: &limit})
	require.NoError(t, err)
	resp, b := h.upload(client, "public", api.JobParams{Model: "small.en"}, bytes.Repeat([]byte("v"), 5000), "")
	h.expectProblem(resp, b, http.StatusRequestEntityTooLarge, api.ProblemTooLarge)
}

// --- fix: worker info metric ---

func TestWorkerInfoExcludesPending(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	pending, _ := h.worker(api.WorkerFacts{Hostname: "unvetted"})
	active, _ := h.activeWorker(api.WorkerFacts{Hostname: "vetted"}, "public")
	h.srv.refreshStats(h.ctx)
	var mb syncBuffer
	h.metrics.WritePrometheus(&mb)
	out := string(mb.Bytes())
	assert.Contains(t, out, `whisper_worker_info{worker="`+active+`"`)
	assert.NotContains(t, out, pending)
}

// --- fix: nosniff ---

func TestNosniffOnEveryResponse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, path := range []string{"/healthz", "/job/nope", "/admin/queues", "/no-such-route"} {
		resp, _ := h.call("GET", path, h.admin, nil)
		assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"), path)
	}
}

// --- fix: authentication bookkeeping ---

// TestClientTouchFailureDoesNotFailAuth: failing to record last use is
// logged, and the request still succeeds.
func TestClientTouchFailureDoesNotFailAuth(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	db, err := sql.Open("sqlite", filepath.Join(h.dir, "wn.db"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(h.ctx, `CREATE TRIGGER refuse_touch BEFORE UPDATE OF last_used_at ON clients
		BEGIN SELECT RAISE(FAIL, 'touch refused'); END`)
	require.NoError(t, err)

	h.expect("GET", "/queue/public", client, nil, http.StatusOK, nil)
	assert.Contains(t, string(h.logs.Bytes()), "recording client use failed")
	assert.Empty(t, h.logs.events(t, obs.EventAuthDenied))
}

// TestAuthStoreErrorIsNotADenial: a failing database is a 500, logged as
// a server error, not as auth.denied.
func TestAuthStoreErrorIsNotADenial(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	require.NoError(t, h.st.Close())
	resp, b := h.call("GET", "/queue/public", client, nil)
	h.expectProblem(resp, b, http.StatusInternalServerError, api.ProblemInternal)
	assert.Empty(t, h.logs.events(t, obs.EventAuthDenied))

	resp, _ = h.call("GET", "/queue/public", "wnc_bogus", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.NotEmpty(t, h.logs.events(t, obs.EventAuthDenied), "refusals are still denials")
}

// --- fix: download counting ---

func TestDownloadsCountOnlyDeliveredGETs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	resp, b := h.putResult(l, wtok, result(l, "hi"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	j := h.job(job.ID)
	require.Equal(t, api.JobDone, j.State)

	path := "/job/" + job.ID + "/transcript"
	resp = h.do(req{method: "HEAD", path: path, token: client})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	since := j.CompletedAt.Add(time.Hour).UTC().Format(http.TimeFormat)
	resp = h.do(req{method: "GET", path: path, token: client, header: map[string]string{"If-Modified-Since": since}})
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	assert.Equal(t, 0, h.job(job.ID).Downloads.Count, "HEAD and 304 are not downloads")

	h.expect("GET", path, client, nil, http.StatusOK, nil)
	resp = h.do(req{method: "GET", path: path, token: client, header: map[string]string{"Range": "bytes=0-9"}})
	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Equal(t, 2, h.job(job.ID).Downloads.Count)
}

// --- fix: background work is owned ---

// TestCloseStopsLoops: Close stops the background loops even when the
// context given to Start is never cancelled, and waits for them.
func TestCloseStopsLoops(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tick := make(chan time.Time)
	srv, err := New(h.srv.cfg, Deps{Store: h.st, Blobs: h.blobs, Logger: h.srv.log, Now: h.clk.Now,
		FreeBytes: func(string) (int64, error) { return 1 << 40, nil }, LookupModel: lookupTestModel,
		NewTicker: func(time.Duration) Ticker { return manualTicker{tick} }})
	require.NoError(t, err)
	require.NoError(t, srv.Start(context.Background()))
	tick <- time.Now() // a loop is running

	closed := make(chan struct{})
	go func() {
		srv.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case tick <- time.Now():
		t.Fatal("a loop survived Close")
	case <-time.After(50 * time.Millisecond):
	}
}

type panicTransport struct{}

func (panicTransport) RoundTrip(*http.Request) (*http.Response, error) { panic("transport exploded") }

// TestModelFetchPanicReleasesWaiters: a panicking download fails its
// waiters instead of hanging them, and wait returns.
func TestModelFetchPanicReleasesWaiters(t *testing.T) {
	t.Parallel()
	c := newModelCache(t.TempDir(), "", &http.Client{Transport: panicTransport{}}, zerolog.Nop())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.ensure(ctx, context.Background(), fakeModel)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "panic")
	c.wait()
	_, err = c.ensure(ctx, ctx, models.Model{Name: "other", File: "x.bin", Size: 1})
	require.Error(t, err)
}

// --- retention boundaries ---

// TestCanonicalRetentionBoundary: canonical audio is kept until exactly
// completed_at + audio_retention.
func TestCanonicalRetentionBoundary(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	resp, b := h.putResult(l, wtok, result(l, "x"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	assertRetentionBoundary(t, h, job.ID, h.srv.cfg.AudioRetention)
}

// TestFailedJobAudioRetention: a failed job's canonical audio follows the
// failed-job retention from when it failed, not the audio retention (#56).
func TestFailedJobAudioRetention(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config, _ *store.Config) {
		c.AudioRetention = time.Hour
		c.FailedRetention = 48 * time.Hour
	})
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	l := h.claim(wid, wtok, "", nil)
	h.expect("POST", "/lease/"+l.ID+"/failure", wtok, api.FailureReport{Reason: "boom"}, http.StatusNoContent, nil)
	require.Equal(t, api.JobFailed, h.job(job.ID).State)
	assertRetentionBoundary(t, h, job.ID, h.srv.cfg.FailedRetention)
}

func assertRetentionBoundary(t *testing.T, h *harness, jobID string, retention time.Duration) {
	t.Helper()
	j := h.job(jobID)
	require.NotNil(t, j.CompletedAt)
	require.False(t, j.Media.AudioPurged)
	purgeAt := j.CompletedAt.Add(retention)
	h.clk.Advance(purgeAt.Sub(h.clk.Now()) - time.Millisecond)
	h.srv.reapOnce(h.ctx)
	assert.False(t, h.job(jobID).Media.AudioPurged, "kept 1 ms before retention ends")
	_, err := os.Stat(blobPath(t, h, jobID, blobs.Canonical))
	assert.NoError(t, err)

	h.clk.Advance(time.Millisecond)
	h.srv.reapOnce(h.ctx)
	assert.True(t, h.job(jobID).Media.AudioPurged, "purged when retention ends")
	_, err = os.Stat(blobPath(t, h, jobID, blobs.Canonical))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestOriginalPurgedOnCanonicalAccept: once a prepare result is accepted
// the original upload is gone at once, without waiting for retention.
func TestOriginalPurgedOnCanonicalAccept(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, []byte("opaque video"))
	l := h.claim(wid, wtok, "", nil)
	require.Equal(t, api.KindPrepare, l.Kind)
	resp, b := h.putFLAC(l, wtok, canonicalFLAC(t, speech(2, media.SampleRate)))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	assert.True(t, h.job(job.ID).Media.OriginalPurged)
	_, err := os.Stat(blobPath(t, h, job.ID, blobs.Original))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestQueuedJobsAreNeverPurged: a job nobody has processed keeps its audio
// and its row however long it waits.
func TestQueuedJobsAreNeverPurged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(2, media.SampleRate)))
	h.clk.Advance(2 * (h.srv.cfg.JobRetention + h.srv.cfg.AudioRetention))
	h.srv.reapOnce(h.ctx)
	j := h.job(job.ID)
	assert.Equal(t, api.JobQueued, j.State)
	assert.False(t, j.Media.AudioPurged)
	_, err := os.Stat(blobPath(t, h, job.ID, blobs.Canonical))
	assert.NoError(t, err)
}
