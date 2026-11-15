package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"

	"github.com/deweysasser/whisper-network/internal/api"
)

// Error is a non-success response from the server. Problem holds the decoded
// application/problem+json body, or a synthesized one when the server (or a
// proxy in front of it) sent something else.
type Error struct {
	Method  string
	Path    string
	Status  int
	Problem api.Problem
}

func (e *Error) Error() string {
	msg := e.Problem.Title
	if e.Problem.Detail != "" {
		msg += ": " + e.Problem.Detail
	}
	if e.Problem.Type != "" {
		msg += " (" + e.Problem.Type + ")"
	}
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, msg)
}

// maxErrorBody bounds how much of an error response is read.
const maxErrorBody = 16 << 10

func decodeError(req *http.Request, resp *http.Response) *Error {
	e := &Error{Method: req.Method, Path: req.URL.Path, Status: resp.StatusCode}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt == api.ProblemContentType || mt == "application/json" {
		_ = json.Unmarshal(body, &e.Problem)
	}
	if e.Problem.Status == 0 {
		e.Problem.Status = resp.StatusCode
	}
	if e.Problem.Title == "" {
		e.Problem.Title = http.StatusText(resp.StatusCode)
		if e.Problem.Detail == "" && mt != api.ProblemContentType {
			e.Problem.Detail = strings.TrimSpace(strings.ToValidUTF8(string(body[:min(len(body), 512)]), ""))
		}
	}
	return e
}

// AsError returns the *Error in err's chain, if any.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// StatusOf returns the HTTP status carried by err, or 0 if err is not an
// *Error.
func StatusOf(err error) int {
	if e, ok := AsError(err); ok {
		return e.Status
	}
	return 0
}

// ProblemType returns the problem type slug carried by err, or "".
func ProblemType(err error) string {
	if e, ok := AsError(err); ok {
		return e.Problem.Type
	}
	return ""
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool { return StatusOf(err) == http.StatusNotFound }

// IsUnauthorized reports a 401: the credentials are unknown or revoked.
func IsUnauthorized(err error) bool { return StatusOf(err) == http.StatusUnauthorized }

// IsLeaseGone reports that a lease is no longer live (410 on a lease
// endpoint): it expired, was cancelled, or its job was deleted. The worker
// must stop work on it. A rejected worker also gets 410; check IsRejected
// first when the difference matters.
func IsLeaseGone(err error) bool { return StatusOf(err) == http.StatusGone }

// IsRejected reports that the worker has been rejected: its token is revoked
// and it must exit.
func IsRejected(err error) bool { return ProblemType(err) == api.ProblemWorkerRejected }

// IsPaused reports a claim refused because the worker is paused (409).
func IsPaused(err error) bool {
	return ProblemType(err) == api.ProblemWorkerPaused ||
		(StatusOf(err) == http.StatusConflict && problemState(err) == string(api.WorkerPaused))
}

// IsPending reports a claim refused because the worker awaits approval (403).
func IsPending(err error) bool {
	return ProblemType(err) == api.ProblemWorkerPending ||
		(StatusOf(err) == http.StatusForbidden && problemState(err) == string(api.WorkerPending))
}

// IsResultInvalid reports a 422 on a result upload: the server rejected the
// result and counted the attempt as failed.
func IsResultInvalid(err error) bool { return StatusOf(err) == http.StatusUnprocessableEntity }

// IsNotDone reports a 409 for a transcript requested before the job finished.
func IsNotDone(err error) bool {
	return ProblemType(err) == api.ProblemNotDone || StatusOf(err) == http.StatusConflict
}

// IsTransient reports an error worth retrying unchanged: a network failure, a
// 5xx, 408 or 429. Context cancellation is not transient.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if e, ok := AsError(err); ok {
		return e.Status >= 500 || e.Status == http.StatusTooManyRequests || e.Status == http.StatusRequestTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, errNoBody) ||
		errors.Is(err, errShortBody) || strings.Contains(err.Error(), "connection reset")
}

func problemState(err error) string {
	if e, ok := AsError(err); ok {
		return e.Problem.State
	}
	return ""
}
