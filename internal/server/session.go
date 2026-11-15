package server

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/blobs"
	"github.com/deweysasser/whisper-network/internal/store"
)

// This file is the seam for the web UI (internal/ui). The UI builds its own
// login pages and templates; the server owns credentials, sessions, CSRF and
// authorization so the UI and the API share one implementation (DESIGN.md
// §5.1: a user may call the API with a session).

// Session cookie and CSRF names.
const (
	// SessionCookieName is the UI session cookie (HttpOnly, SameSite=Lax,
	// Secure when Config.CookieSecure).
	SessionCookieName = "wn_session"
	// CSRFHeader carries the session's CSRF token on mutating API calls
	// authenticated by the session cookie (htmx can send it on every request).
	CSRFHeader = "X-CSRF-Token"
	// CSRFFormField carries the CSRF token in HTML form posts.
	CSRFFormField = "csrf"
)

// ErrBadCredentials is returned by Login for an unknown user, a wrong
// password or a disabled account, deliberately without saying which.
var ErrBadCredentials = errors.New("invalid username or password")

// Store returns the server's store, for UI reads.
func (s *Server) Store() *store.Store { return s.st }

// Blobs returns the server's blob store.
func (s *Server) Blobs() *blobs.Store { return s.blobs }

// Ops returns the audited administrative operations, for UI mutations. The
// UI must authorize first (Principal plus auth.Can / auth.CanGrantTo /
// auth.CanDelegateAtCreation / auth.CanApproveWorker), exactly as the admin
// API does.
func (s *Server) Ops() *Ops { return s.ops }

// Notify wakes waiting worker claims; call it after changes that may make
// work claimable (approving a worker, granting a queue).
func (s *Server) Notify() { s.disp.Notify() }

var dummyHash = sync.OnceValue(func() string {
	h, _ := auth.HashPassword("timing-equalizer", auth.DefaultPasswordParams())
	return h
})

// Login verifies a username and password. Unknown users still cost one
// password verification so response timing does not reveal which usernames
// exist. Verification holds a PasswordSlot; when the server is saturated the
// error is a 503 problem (ErrorStatus maps it).
func (s *Server) Login(ctx context.Context, username, password string) (*store.User, error) {
	release, err := s.PasswordSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	u, err := s.st.UserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		_, _ = auth.VerifyPassword(password, dummyHash())
		return nil, ErrBadCredentials
	}
	if err != nil {
		return nil, err
	}
	ok, err := auth.VerifyPassword(password, u.PasswordHash)
	if err != nil || !ok || u.DisabledAt != nil {
		return nil, ErrBadCredentials
	}
	return u, nil
}

// StartSession creates a session for user and sets its cookie, audited as
// auth.login. user must be as returned by Login (or updated by
// ChangePassword): if its password hash is no longer current, or the user
// was disabled meanwhile, no session is created and the error is
// ErrBadCredentials. Any session the request already carried is deleted in the
// same transaction (rotation on login defeats session fixation). The
// returned session's CSRF field is the token to embed in forms.
func (s *Server) StartSession(w http.ResponseWriter, r *http.Request, user *store.User) (*store.Session, error) {
	ctx := r.Context()
	oldID := ""
	if c, err := r.Cookie(SessionCookieName); err == nil {
		if id, err := auth.ParseSessionID(c.Value); err == nil {
			oldID = id
		}
	}
	cookie, err := auth.NewSessionID()
	if err != nil {
		return nil, err
	}
	csrf, err := auth.NewCSRFToken()
	if err != nil {
		return nil, err
	}
	sess := store.Session{ID: auth.HashToken(cookie), UserID: user.ID, CSRF: csrf,
		ExpiresAt: s.now().Add(s.cfg.SessionTTL)}
	if err := s.ops.CreateSession(ctx, user, sess, oldID, s.clientIP(r)); err != nil {
		if errors.Is(err, store.ErrCredentialsChanged) {
			return nil, ErrBadCredentials // reset or disabled since Login verified it
		}
		return nil, err
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: cookie, Path: "/", Expires: sess.ExpiresAt,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	return &sess, nil
}

// SessionFromRequest returns the request's live session and its user, or
// (nil, nil, nil) when there is no valid session cookie.
func (s *Server) SessionFromRequest(r *http.Request) (*store.Session, *store.User, error) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return nil, nil, nil
	}
	id, err := auth.ParseSessionID(c.Value)
	if err != nil {
		return nil, nil, nil
	}
	sess, err := s.st.Session(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	u, err := s.st.UserByID(r.Context(), sess.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return sess, u, nil
}

// EndSession deletes the request's session (audited as auth.logout) and
// clears its cookie.
func (s *Server) EndSession(w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(SessionCookieName); err == nil {
		if id, err := auth.ParseSessionID(c.Value); err == nil {
			if err := s.ops.Logout(r.Context(), id); err != nil {
				return err
			}
		}
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	return nil
}

// CheckCSRF reports whether r carries sess's CSRF token, in the CSRFHeader
// header or, for url-encoded form posts, the CSRFFormField field.
func (s *Server) CheckCSRF(r *http.Request, sess *store.Session) bool {
	tok := r.Header.Get(CSRFHeader)
	if tok == "" {
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/x-www-form-urlencoded" {
			tok = r.PostFormValue(CSRFFormField)
		}
	}
	return tok != "" && auth.Equal(tok, sess.CSRF)
}

// RequestPrincipal authenticates r (bearer token or session cookie) and
// returns its principal with grants loaded. Errors are problems suitable
// for an API response; the UI usually redirects to its login page instead.
func (s *Server) RequestPrincipal(r *http.Request) (*Principal, error) { return s.authenticate(r) }

// PrincipalForUser returns the principal of a user, with grants loaded.
func (s *Server) PrincipalForUser(ctx context.Context, u *store.User) (*Principal, error) {
	return s.userPrincipal(ctx, u)
}

// ChangePassword sets a user's password (clearing must_change_password) and
// ends all of the user's sessions, audited as user.change_password; the
// caller starts a fresh one. verifiedHash is the PasswordHash of the user
// Login returned when the current password was checked: if that hash is no
// longer current, or the user was disabled meanwhile, nothing changes and
// the error is ErrBadCredentials. The hash holds a PasswordSlot (inside
// Ops), so a saturated server answers 503.
func (s *Server) ChangePassword(ctx context.Context, u *store.User, verifiedHash, password string) error {
	err := s.ops.ChangePassword(ctx, u, verifiedHash, password)
	if errors.Is(err, store.ErrCredentialsChanged) {
		return ErrBadCredentials // reset or disabled since the check
	}
	return err
}

// ClientIP returns the caller's address as the server sees it, honouring
// Config.TrustProxy exactly as the API does (for UI login rate limiting).
func (s *Server) ClientIP(r *http.Request) string { return s.clientIP(r) }

// Logger returns the server's event logger.
func (s *Server) Logger() zerolog.Logger { return s.log }

// Now returns the server's clock reading.
func (s *Server) Now() time.Time { return s.now() }

// DeleteJob deletes a job exactly as DELETE /job/{id} does (row, live
// leases, blobs, job.deleted event and audit entry). The caller must have
// authorized p for auth.ActDeleteJob on the job.
func (s *Server) DeleteJob(ctx context.Context, p *Principal, jobID string) error {
	return s.deleteJob(ctx, p.Actor(), jobID)
}

// Busy reports whether err is the server's "busy, retry later" refusal (a
// saturated PasswordSlot or analysis gate), and if so how many seconds the
// caller should wait before retrying.
func Busy(err error) (retryAfter int, ok bool) {
	if err == nil {
		return 0, false
	}
	p := asProblem(err)
	if p.status != http.StatusServiceUnavailable || p.retryAfter <= 0 {
		return 0, false
	}
	return p.retryAfter, true
}

// ErrorStatus maps an error returned by the server's operations (Ops,
// DeleteJob, store calls) to the HTTP status and caller-safe detail the API
// would send. Internal errors map to 500 with a generic detail; their text
// is never exposed.
func ErrorStatus(err error) (status int, detail string) {
	p := asProblem(err)
	if p.typ == api.ProblemInternal {
		return p.status, "internal error"
	}
	return p.status, p.detail
}
