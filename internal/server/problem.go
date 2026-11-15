package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/store"
)

// problem is an error that maps to an RFC 9457 response. Handlers return
// it (or a store error, mapped by asProblem) and writeError renders it.
type problem struct {
	status int
	typ    string
	detail string
	state  string // worker state extension member, if any
	// retryAfter, when positive, is sent as Retry-After seconds.
	retryAfter int
}

func (p *problem) Error() string { return p.typ + ": " + p.detail }

func newProblem(status int, typ, detail string) *problem {
	return &problem{status: status, typ: typ, detail: detail}
}

func badRequest(detail string) *problem {
	return newProblem(http.StatusBadRequest, api.ProblemBadRequest, detail)
}

func notFound() *problem { return newProblem(http.StatusNotFound, api.ProblemNotFound, "not found") }

func forbidden(detail string) *problem {
	return newProblem(http.StatusForbidden, api.ProblemForbidden, detail)
}

func conflict(detail string) *problem {
	return newProblem(http.StatusConflict, api.ProblemConflict, detail)
}

func leaseGone() *problem {
	return newProblem(http.StatusGone, api.ProblemLeaseGone, "lease is no longer live; stop work")
}

// asProblem maps an error to a problem: problems pass through, store
// sentinels get their HTTP meaning, and anything else is internal.
func asProblem(err error) *problem {
	var p *problem
	switch {
	case errors.As(err, &p):
		return p
	case errors.Is(err, store.ErrNotFound):
		return notFound()
	case errors.Is(err, store.ErrLeaseGone):
		return leaseGone()
	case errors.Is(err, store.ErrConflict):
		return conflict(err.Error())
	case errors.Is(err, store.ErrInvalid):
		return badRequest(err.Error())
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return newProblem(http.StatusRequestEntityTooLarge, api.ProblemTooLarge,
			"request body exceeds "+strconv.FormatInt(mbe.Limit, 10)+" bytes")
	}
	return newProblem(http.StatusInternalServerError, api.ProblemInternal, "internal error")
}

// writeProblem renders p as application/problem+json.
func writeProblem(w http.ResponseWriter, p *problem) {
	h := w.Header()
	h.Set("Content-Type", api.ProblemContentType)
	h.Set("Cache-Control", "no-store")
	if p.retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(p.retryAfter))
	}
	w.WriteHeader(p.status)
	_ = json.NewEncoder(w).Encode(api.Problem{
		Type: p.typ, Title: http.StatusText(p.status), Status: p.status, Detail: p.detail, State: p.state,
	})
}

// writeError renders err. Internal errors are logged with the request's
// route; their text is never sent to the caller.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	p := asProblem(err)
	if p.status >= 500 && p.typ == api.ProblemInternal {
		log := s.log
		log.Error().Err(err).Str(obs.FieldRoute, routeOf(r)).Str(obs.FieldMethod, r.Method).Msg("request failed")
	}
	writeProblem(w, p)
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// maxJSONBody bounds ordinary JSON request bodies.
const maxJSONBody = 1 << 20

// decodeJSON reads a bounded JSON body into v, rejecting unknown fields so
// typos in client and admin requests fail loudly. An empty body is allowed
// when optional is true.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, optional bool) error {
	return decodeBody(w, r, v, optional, maxJSONBody, true)
}

// decodeWorkerJSON is decodeJSON for the worker protocol, which tolerates
// unknown fields: a fleet of ad-hoc machines upgrades unevenly, and a newer
// worker must still be able to talk to an older server.
func decodeWorkerJSON(w http.ResponseWriter, r *http.Request, v any, optional bool, limit int64) error {
	return decodeBody(w, r, v, optional, limit, false)
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any, optional bool, limit int64, strict bool) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return nil
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return badRequest("invalid JSON body: " + err.Error())
	}
	if dec.More() {
		return badRequest("trailing data after JSON body")
	}
	return nil
}
