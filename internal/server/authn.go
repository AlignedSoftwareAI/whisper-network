package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/store"
)

// Principal is an authenticated caller: the auth.Principal that Can
// evaluates (grants with queue scopes by name), plus the stored record
// behind it. Exactly one of User, Client and Worker is set.
type Principal struct {
	auth.Principal
	// Name is the human-readable actor used in logs and the audit table:
	// "user:<username>", "client:<id>" or "worker:<id>".
	Name string
	// Grant is the stored grant principal ("user:<id>", "client:<id>",
	// "worker:<id>"), used as granted_by.
	Grant   string
	User    *store.User
	Client  *store.Client
	Worker  *store.Worker
	Session *store.Session // set when authenticated by session cookie
}

// kinds is a set of principal kinds an endpoint accepts.
type kinds []auth.PrincipalKind

var (
	anyHuman   = kinds{auth.KindUser, auth.KindClient}
	workerOnly = kinds{auth.KindWorker}
	anyKind    = kinds{auth.KindUser, auth.KindClient, auth.KindWorker}
)

// handlerFunc is an authenticated handler. Returning an error renders it as
// a problem; handlers write their own success responses.
type handlerFunc func(w http.ResponseWriter, r *http.Request, p *Principal) error

// authed authenticates the request, checks the principal kind, and calls h.
func (s *Server) authed(allow kinds, h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.authenticate(r)
		if err != nil {
			// Only a refusal is a denial; a failing database is a server
			// fault, which writeError logs as such.
			if _, refused := errors.AsType[*problem](err); refused {
				s.denied(r, nil, err.Error())
			}
			s.writeError(w, r, err)
			return
		}
		infoFrom(r.Context()).principal = p.Name
		if !slices.Contains(allow, p.Kind) {
			s.writeError(w, r, s.deny(r, p, "principal kind "+string(p.Kind)+" not accepted"))
			return
		}
		if p.Session != nil && !isSafeMethod(r.Method) && !s.CheckCSRF(r, p.Session) {
			s.writeError(w, r, s.deny(r, p, "missing or bad CSRF token"))
			return
		}
		if err := h(w, r, p); err != nil {
			s.writeError(w, r, err)
		}
	})
}

// public wraps an unauthenticated handler.
func (s *Server) public(h func(w http.ResponseWriter, r *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.writeError(w, r, err)
		}
	})
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func unauthorized(detail string) *problem {
	return newProblem(http.StatusUnauthorized, api.ProblemUnauthorized, detail)
}

// deny logs auth.denied and returns a 403 problem.
func (s *Server) deny(r *http.Request, p *Principal, reason string) *problem {
	s.denied(r, p, reason)
	return forbidden(reason)
}

// hide logs auth.denied and returns 404, for resources whose existence must
// not leak (jobs, DESIGN.md §5.3).
func (s *Server) hide(r *http.Request, p *Principal, reason string) *problem {
	s.denied(r, p, reason)
	return notFound()
}

func (s *Server) denied(r *http.Request, p *Principal, reason string) {
	name := ""
	if p != nil {
		name = p.Name
	}
	obs.Warn(&s.log, obs.EventAuthDenied).Str(obs.FieldPrincipal, name).Str(obs.FieldRoute, routeOf(r)).
		Str(obs.FieldReason, reason).Msg("request denied")
}

// ProblemPasswordChangeRequired is the problem type of an API request made
// with the session of a user who must change their password first.
const ProblemPasswordChangeRequired = "password-change-required"

// clientTouchInterval throttles last_used_at updates so polling clients do
// not turn every read into a database write.
const clientTouchInterval = time.Minute

// authenticate resolves the bearer token or session cookie of r.
func (s *Server) authenticate(r *http.Request) (*Principal, error) {
	ctx := r.Context()
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, token, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return nil, unauthorized("unsupported authorization scheme")
		}
		return s.authenticateToken(ctx, strings.TrimSpace(token))
	}
	sess, user, err := s.SessionFromRequest(r)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, unauthorized("authentication required")
	}
	if user.MustChangePassword {
		// An administrator set this password; until the user picks their
		// own (in the UI, which handles that flow itself) the session is
		// good for nothing else.
		return nil, newProblem(http.StatusForbidden, ProblemPasswordChangeRequired,
			"the password must be changed (in the UI) before this session can be used")
	}
	p, err := s.userPrincipal(ctx, user)
	if err != nil {
		return nil, err
	}
	p.Session = sess
	return p, nil
}

func (s *Server) authenticateToken(ctx context.Context, token string) (*Principal, error) {
	kind, hash, err := auth.ParseToken(token)
	if err != nil {
		return nil, unauthorized("invalid token")
	}
	switch kind {
	case auth.KindClient:
		c, err := s.st.ClientByTokenHash(ctx, hash)
		if errors.Is(err, store.ErrNotFound) {
			return nil, unauthorized("invalid token")
		}
		if err != nil {
			return nil, err
		}
		if c.LastUsedAt == nil || s.now().Sub(*c.LastUsedAt) >= clientTouchInterval {
			// last_used_at is bookkeeping: failing to record it must not
			// fail an otherwise valid request.
			if err := s.st.TouchClient(ctx, c.ID); err != nil && ctx.Err() == nil {
				s.log.Warn().Err(err).Str(obs.FieldClientID, c.Principal()).Msg("recording client use failed")
			}
		}
		p := &Principal{Principal: auth.Principal{Kind: auth.KindClient, ID: c.ID},
			Name: c.Principal(), Grant: c.Principal(), Client: c}
		return p, s.loadGrants(ctx, p)
	case auth.KindWorker:
		wk, err := s.st.WorkerByTokenHash(ctx, hash)
		if errors.Is(err, store.ErrNotFound) {
			rejected, rerr := s.st.WorkerTokenRejected(ctx, hash)
			if rerr != nil {
				return nil, rerr
			}
			if rejected {
				p := newProblem(http.StatusGone, api.ProblemWorkerRejected, "worker was rejected; exit")
				p.state = string(api.WorkerRejected)
				return nil, p
			}
			return nil, unauthorized("invalid token")
		}
		if err != nil {
			return nil, err
		}
		p := &Principal{Principal: auth.Principal{Kind: auth.KindWorker, ID: wk.ID},
			Name: wk.Principal(), Grant: wk.Principal(), Worker: wk}
		return p, s.loadGrants(ctx, p)
	}
	return nil, unauthorized("invalid token")
}

func (s *Server) userPrincipal(ctx context.Context, u *store.User) (*Principal, error) {
	p := &Principal{Principal: auth.Principal{Kind: auth.KindUser, ID: u.Principal()},
		Name: "user:" + u.Username, Grant: u.Principal(), User: u}
	return p, s.loadGrants(ctx, p)
}

// loadGrants loads p's grants, translating stored queue scopes
// ("queue:<id>") into the by-name form auth evaluates.
func (s *Server) loadGrants(ctx context.Context, p *Principal) error {
	gs, err := s.ops.WireGrants(ctx, p.Grant)
	if err != nil {
		return err
	}
	p.Grants = gs
	return nil
}

// parseWait parses a claim's ?wait= value: a Go duration ("30s") or plain
// seconds ("30"), capped at max.
func parseWait(v string, max time.Duration) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		secs, serr := strconv.ParseFloat(v, 64)
		if serr != nil || secs < 0 {
			return 0, badRequest("wait must be a duration such as 30s")
		}
		d = time.Duration(secs * float64(time.Second))
	}
	if d < 0 {
		return 0, badRequest("wait must not be negative")
	}
	return min(d, max), nil
}
