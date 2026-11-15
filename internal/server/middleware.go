package server

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/rs/zerolog"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/obs"
)

func newMetricsSet() *metrics.Set { return metrics.NewSet() }

// reqInfo is filled in while a request is handled and read by the access
// log afterwards. It lives in the request context as a pointer so handlers
// deeper in the chain can set it without replacing the request.
type reqInfo struct {
	principal string
}

type reqInfoKey struct{}

func infoFrom(ctx context.Context) *reqInfo {
	if ri, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		return ri
	}
	return &reqInfo{}
}

// statusWriter records the status and body size of a response. Unwrap lets
// http.ResponseController reach the underlying writer (Flush, deadlines).
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// ReadFrom keeps net/http's sendfile path for large downloads (originals,
// models) while still counting bytes.
func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := io.Copy(w.ResponseWriter, r)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// countingBody counts request body bytes read.
type countingBody struct {
	io.ReadCloser
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

// knownMethods bounds the method label of the request metrics.
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

// accessLog emits one http.request event per request and records the HTTP
// metrics. The route is the matched pattern (never the raw path, which
// carries job and lease IDs); heartbeats log at debug to keep volume down.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ri := &reqInfo{}
		r = r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, ri))
		body := &countingBody{ReadCloser: r.Body}
		r.Body = body
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			d := time.Since(start)
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			route := routeOf(r)
			method := r.Method
			if !knownMethods[method] {
				method = "other"
			}
			s.metrics.HTTPRequest(route, method, status, d)
			ev := obs.Info
			if strings.HasSuffix(route, "/heartbeat") {
				ev = obs.Debug
			}
			ev(&s.log, obs.EventHTTPRequest).
				Str(obs.FieldMethod, method).
				Str(obs.FieldRoute, route).
				Int(obs.FieldStatus, status).
				Int64(obs.FieldDurationMS, d.Milliseconds()).
				Int64(obs.FieldBytesIn, body.n).
				Int64(obs.FieldBytesOut, sw.bytes).
				Str(obs.FieldPrincipal, ri.principal).
				Str(obs.FieldRemote, s.clientIP(r)).
				Msg("request")
		}()
		next.ServeHTTP(sw, r)
	})
}

// routeOf returns the matched route pattern without its method ("GET
// /job/{id}" becomes "/job/{id}"), or "unmatched".
func routeOf(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}
	if _, path, ok := strings.Cut(r.Pattern, " "); ok {
		return path
	}
	return r.Pattern
}

// recoverer turns a handler panic into a 500 problem. http.ErrAbortHandler
// is re-raised: it is how a handler aborts a response already under way.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			s.log.Error().Interface("panic", v).Str(obs.FieldRoute, routeOf(r)).Msg("handler panic")
			writeProblem(w, newProblem(http.StatusInternalServerError, api.ProblemInternal, "internal error"))
		}()
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the caller's IP address. X-Forwarded-For is believed
// only with Config.TrustProxy, and then only its rightmost entry: that is the
// address the trusted proxy itself saw, which a client cannot forge (entries
// to its left are client-supplied).
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			parts := strings.Split(xff[len(xff)-1], ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// newStdLogger adapts the zerolog logger for net/http's ErrorLog (TLS
// handshake failures and the like), at warn level.
func newStdLogger(l zerolog.Logger) *log.Logger {
	return log.New(zerologWriter{l}, "", 0)
}

type zerologWriter struct{ l zerolog.Logger }

func (z zerologWriter) Write(p []byte) (int, error) {
	z.l.Warn().Msg(strings.TrimSpace(string(p)))
	return len(p), nil
}
