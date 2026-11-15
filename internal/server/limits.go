package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
)

// This file holds the server's admission control: bounded concurrency for
// expensive work (password hashing, media analysis), the free-space budget
// of uploads in progress, the set of uploads the orphan sweep must not
// touch, and per-request I/O deadlines.

// --- concurrency gates ---

// gate is a counting semaphore for expensive work.
type gate struct{ slots chan struct{} }

func newGate(n int) *gate { return &gate{slots: make(chan struct{}, max(n, 1))} }

// acquire takes a slot, waiting up to wait. When none frees up in time it
// returns a 503 problem telling the caller to retry after retryAfter; when
// ctx ends first it returns errClientGone.
func (g *gate) acquire(ctx context.Context, wait time.Duration, retryAfter int) (release func(), err error) {
	select {
	case g.slots <- struct{}{}:
		return g.release, nil
	default:
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case g.slots <- struct{}{}:
		return g.release, nil
	case <-ctx.Done():
		return nil, errClientGone
	case <-t.C:
		p := newProblem(http.StatusServiceUnavailable, api.ProblemRateLimited, "the server is busy; retry later")
		p.retryAfter = retryAfter
		return nil, p
	}
}

func (g *gate) release() { <-g.slots }

// errClientGone is returned when the caller went away while waiting; the
// response is never seen, it only must not be logged as a server fault.
var errClientGone = newProblem(http.StatusBadRequest, api.ProblemBadRequest, "request cancelled")

// passwordSlots sizes the password gate: as many argon2id computations as
// fit in budget, within [2, GOMAXPROCS] (argon2id is also CPU bound).
func passwordSlots(budget int64, p auth.PasswordParams) int {
	// Unknown-user logins verify against a hash with the default costs.
	kib := max(int64(p.MemoryKiB), int64(auth.DefaultPasswordParams().MemoryKiB), 1)
	return max(2, min(runtime.GOMAXPROCS(0), int(budget/(kib<<10))))
}

// PasswordSlot reserves capacity for one argon2id hash or verification. Call
// release when done. Every password hash computed on behalf of a request must
// hold a slot: each costs tens of MiB, and unbounded concurrency lets a burst
// of logins exhaust memory. When the server is saturated for longer than
// Config.BusyWait the error is a 503 problem.
func (s *Server) PasswordSlot(ctx context.Context) (release func(), err error) {
	return s.passwords.acquire(ctx, s.cfg.BusyWait, 5)
}

// analysisSlot reserves a CPU for media validation or encoding on behalf of
// principal (its stored grant principal). Each principal may hold at most
// Config.AnalysesPerPrincipal slots, so one caller cannot occupy every CPU
// with hostile media; beyond that it waits up to BusyWait, then gets 429.
// The global gate (GOMAXPROCS slots) then bounds the server as a whole.
func (s *Server) analysisSlot(ctx context.Context, principal string) (release func(), err error) {
	mine, err := s.analysisBy.acquire(ctx, principal, s.cfg.AnalysesPerPrincipal, s.cfg.BusyWait,
		"too many media analyses in progress for this caller; retry later")
	if err != nil {
		return nil, err
	}
	all, err := s.analysis.acquire(ctx, s.cfg.BusyWait, 30)
	if err != nil {
		mine()
		return nil, err
	}
	return func() { all(); mine() }, nil
}

// keyedGate bounds concurrent work per key (a principal): a counting
// semaphore per key, created on first use and dropped when idle.
type keyedGate struct {
	mu   sync.Mutex
	sems map[string]*keySem
}

type keySem struct {
	slots chan struct{}
	refs  int // holders plus waiters
}

// acquire takes one of limit slots for key, waiting up to wait. When none
// frees up in time it returns a 429 problem with detail; when ctx ends
// first, errClientGone.
func (k *keyedGate) acquire(ctx context.Context, key string, limit int, wait time.Duration, detail string) (release func(), err error) {
	k.mu.Lock()
	if k.sems == nil {
		k.sems = map[string]*keySem{}
	}
	sem := k.sems[key]
	if sem == nil {
		sem = &keySem{slots: make(chan struct{}, max(limit, 1))}
		k.sems[key] = sem
	}
	sem.refs++
	k.mu.Unlock()
	done := func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		if sem.refs--; sem.refs == 0 {
			delete(k.sems, key)
		}
	}
	release = func() { <-sem.slots; done() }
	select {
	case sem.slots <- struct{}{}:
		return release, nil
	default:
	}
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case sem.slots <- struct{}{}:
			return release, nil
		case <-ctx.Done():
			done()
			return nil, errClientGone
		case <-t.C:
		}
	}
	done()
	return nil, tooMany(detail, 30)
}

// tooMany is a 429 problem asking the caller to retry after retryAfter
// seconds.
func tooMany(detail string, retryAfter int) *problem {
	p := newProblem(http.StatusTooManyRequests, api.ProblemRateLimited, detail)
	p.retryAfter = retryAfter
	return p
}

// ctxReader fails reads once ctx is done, so a long decode stops soon after
// its client disconnects.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// --- disk budget ---

// diskBudget tracks free space promised to uploads in progress, so that
// concurrent uploads that each fit cannot together overshoot MinFreeBytes.
type diskBudget struct {
	mu       sync.Mutex
	reserved int64
}

// reservation is one upload's outstanding promise. It is taken in steps of
// reserveStep as the upload's bytes arrive, never for the whole size cap or
// the declared length up front, so an upload holds at most what it has been
// sent plus one step; it shrinks as the upload writes (the bytes then show
// up in the free-space probe instead).
type reservation struct {
	s    *Server
	left int64
}

// reserveStep is how much an upload reserves at a time, and so also how
// often (in bytes written) it re-probes free space, catching consumption the
// budget does not know about. It is small so that slow uploads, each
// holding one step it has not yet been sent, cannot together crowd out the
// headroom others need: a principal, capped at UploadsPerPrincipal uploads,
// holds at most that many unsent steps.
const reserveStep = 1 << 20

func storageFull(detail string) *problem {
	return newProblem(http.StatusInsufficientStorage, api.ProblemStorageFull, detail)
}

// reserveDisk starts a reservation of n bytes (at most reserveStep; zero
// only checks that the disk is above MinFreeBytes now), or returns a 507
// problem when the data directory cannot hold them on top of every other
// promise while keeping MinFreeBytes free.
func (s *Server) reserveDisk(n int64) (*reservation, error) {
	r := &reservation{s: s}
	if err := r.grow(n); err != nil {
		return nil, err
	}
	return r, nil
}

// grow promises n more bytes, re-probing free space.
func (r *reservation) grow(n int64) error {
	s := r.s
	free, err := s.freeBytes(s.cfg.DataDir)
	if err != nil {
		s.log.Warn().Err(err).Msg("free space probe failed")
		return storageFull("cannot determine free space")
	}
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	if avail := free - s.disk.reserved - s.cfg.MinFreeBytes; n > avail {
		return storageFull(fmt.Sprintf("not enough free space for %d more bytes", n))
	}
	s.disk.reserved += n
	r.left += n
	return nil
}

// couldFit returns a 507 problem when a body of declared length n (if
// known) could not fit above MinFreeBytes even with every other upload's
// promise released. It reserves nothing: an upload that could fit is
// admitted and reserves as its bytes arrive.
func (s *Server) couldFit(n int64) error {
	if n <= 0 {
		return nil
	}
	free, err := s.freeBytes(s.cfg.DataDir)
	if err != nil {
		s.log.Warn().Err(err).Msg("free space probe failed")
		return storageFull("cannot determine free space")
	}
	// The body includes multipart framing and the params part, which are
	// not written to disk; allow for them so a fitting upload is not refused.
	if n-multipartSlack > free-s.cfg.MinFreeBytes {
		return storageFull(fmt.Sprintf("not enough free space for a %d byte upload", n))
	}
	return nil
}

func (r *reservation) consume(n int64) {
	b := &r.s.disk
	b.mu.Lock()
	defer b.mu.Unlock()
	d := min(n, r.left)
	r.left -= d
	b.reserved -= d
}

// release returns what is left of the promise. It is idempotent.
func (r *reservation) release() { r.consume(r.left) }

// errDiskLow aborts a write that would take the disk below MinFreeBytes.
var errDiskLow = errors.New("free space fell below the minimum")

// guardedWriter writes an upload, reserving space a step ahead of the bytes
// it writes. Bytes the disk cannot hold abort the write with errDiskLow.
type guardedWriter struct {
	w   io.Writer
	res *reservation
}

func (g *guardedWriter) Write(p []byte) (int, error) {
	if short := int64(len(p)) - g.res.left; short > 0 {
		// A whole step ahead if it fits; near the floor, just these bytes.
		if g.res.grow(max(short, reserveStep)) != nil && g.res.grow(short) != nil {
			return 0, errDiskLow
		}
	}
	n, err := g.w.Write(p)
	g.res.consume(int64(n))
	return n, err
}

// --- uploads per principal ---

// uploadCounts counts each principal's uploads in progress.
type uploadCounts struct {
	mu sync.Mutex
	n  map[string]int
}

// begin admits one more upload by principal unless it already has limit in
// progress, in which case it returns a 429 problem. Call done when the
// upload finishes.
func (u *uploadCounts) begin(principal string, limit int) (done func(), err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.n == nil {
		u.n = map[string]int{}
	}
	if limit > 0 && u.n[principal] >= limit {
		return nil, tooMany(fmt.Sprintf("at most %d uploads may be in progress at once per caller", limit), 30)
	}
	u.n[principal]++
	return func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.n[principal]--; u.n[principal] <= 0 {
			delete(u.n, principal)
		}
	}, nil
}

// --- uploads in progress ---

// inflightJobs is the set of job ids whose upload is still being received:
// their directories exist before their rows do, and the orphan sweep must
// leave them alone however slowly they arrive.
type inflightJobs struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

// add registers id and returns the function that removes it.
func (f *inflightJobs) add(id string) (done func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ids == nil {
		f.ids = map[string]struct{}{}
	}
	f.ids[id] = struct{}{}
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.ids, id)
	}
}

func (f *inflightJobs) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.ids[id]
	return ok
}

// --- response headers ---

// secureHeaders marks every response nosniff, so a browser never renders a
// transcript or a problem document as HTML.
func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// --- I/O deadlines ---

// The http.Server's ReadTimeout is a deadline for the whole request read,
// and net/http cancels the request context when it passes, even after the
// body is consumed. That fits small requests but not long-polls, streaming
// downloads, analysis after an upload or large uploads, so deadlines
// adjusts it per request:
//
//   - a bodiless request has its read deadline cleared once its headers are
//     in (ReadHeaderTimeout already bounded them);
//   - a request with a body keeps the deadline until the body is read to
//     EOF, then it is cleared;
//   - an authenticated streaming upload calls streamBody to switch to a
//     progress-based deadline: no stall longer than ReadTimeout, and at
//     least MinTransferRate averaged over every sliding rateWindow.
func (s *Server) deadlines(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if r.ContentLength == 0 {
			_ = rc.SetReadDeadline(time.Time{})
		} else {
			// The request is modified in place, not copied with
			// WithContext: the access log reads the matched pattern from
			// the request it passed down.
			r.Body = &deadlineBody{ReadCloser: r.Body, rc: rc}
		}
		next.ServeHTTP(w, r)
	})
}

// deadlineBody clears the read deadline at EOF and, in streaming mode,
// pushes it forward as data arrives. It is read by one goroutine.
type deadlineBody struct {
	io.ReadCloser
	rc *http.ResponseController

	idle     time.Duration // streaming: the most a read may stall
	hardStop time.Time     // streaming: the latest the body may finish
	extended time.Time     // when the deadline was last pushed
	done     bool

	// Streaming: the sustained-rate check. total counts body bytes; marks
	// are (time, total) checkpoints at least window/rateMarks apart, oldest
	// first, covering a little more than one window.
	rate   int64 // bytes per second; 0 disables the check
	window time.Duration
	total  int64
	marks  []rateMark
	now    func() time.Time
}

type rateMark struct {
	at time.Time
	n  int64
}

// rateMarks is how many checkpoints span one rate window: the window slides
// in steps of window/rateMarks.
const rateMarks = 8

// errTooSlow ends a streaming body whose sustained rate fell below
// MinTransferRate.
var errTooSlow = errors.New("upload is slower than the minimum transfer rate")

// tooSlow records n more bytes at now and reports whether the bytes of the
// last full window fall short of rate. It runs on every read, so a peer
// cannot hold a slot by trickling a byte just often enough to beat the
// stall timeout.
func (b *deadlineBody) tooSlow(n int, now time.Time) bool {
	b.total += int64(n)
	step := b.window / rateMarks
	if last := b.marks[len(b.marks)-1]; now.Sub(last.at) >= step {
		b.marks = append(b.marks, rateMark{at: now, n: b.total})
	}
	// Drop checkpoints so that the oldest is the newest one at least a
	// window old.
	for len(b.marks) > 1 && now.Sub(b.marks[1].at) >= b.window {
		b.marks = b.marks[1:]
	}
	first := b.marks[0]
	age := now.Sub(first.at)
	if age < b.window {
		return false
	}
	return float64(b.total-first.n) < float64(b.rate)*age.Seconds()
}

// minRateWindow is the shortest window the sustained-rate check averages
// over, so a brief stall (a congested link, a paused laptop lid) is not
// fatal by itself.
const minRateWindow = 30 * time.Second

// deadlineRefresh limits how often a streaming read moves the deadline
// (at most a quarter of the idle allowance apart).
const deadlineRefresh = time.Second

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if !b.done && b.rate > 0 && err != io.EOF && b.tooSlow(n, b.now()) {
		b.done = true
		return n, errTooSlow
	}
	switch {
	case b.done:
	case err == io.EOF:
		b.done = true
		_ = b.rc.SetReadDeadline(time.Time{})
	case n > 0 && b.idle > 0:
		if now := time.Now(); now.Sub(b.extended) >= min(deadlineRefresh, b.idle/4) {
			b.extended = now
			_ = b.rc.SetReadDeadline(minTime(now.Add(b.idle), b.hardStop))
		}
	}
	return n, err
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// streamBody lets an authenticated upload of up to maxBytes outlive
// ReadTimeout: the body must keep arriving (no stall longer than
// ReadTimeout), sustain MinTransferRate over every sliding window of
// max(ReadTimeout, minRateWindow) (reads then fail with errTooSlow), and
// finish within ReadTimeout + maxBytes/MinTransferRate. Call it before
// wrapping r.Body (MaxBytesReader).
func (s *Server) streamBody(r *http.Request, maxBytes int64) {
	b, ok := r.Body.(*deadlineBody)
	if !ok || b.done {
		return
	}
	now := time.Now()
	b.idle = s.cfg.ReadTimeout
	b.hardStop = now.Add(s.transferTime(maxBytes))
	b.extended = now
	b.rate = s.cfg.MinTransferRate
	b.window = max(s.cfg.ReadTimeout, minRateWindow)
	b.now = time.Now
	b.marks = []rateMark{{at: b.now()}}
	_ = b.rc.SetReadDeadline(minTime(now.Add(b.idle), b.hardStop))
}

// transferTime is how long moving n bytes may take at MinTransferRate, plus
// ReadTimeout of slack.
func (s *Server) transferTime(n int64) time.Duration {
	return s.cfg.ReadTimeout + time.Duration(float64(n)/float64(s.cfg.MinTransferRate)*float64(time.Second))
}

// extendWrite sets a write deadline for a response of about n bytes, so a
// client that stops reading a download is cut off rather than holding the
// connection forever. Small responses need none: they fit in socket buffers.
func (s *Server) extendWrite(w http.ResponseWriter, n int64) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.transferTime(n)))
}
