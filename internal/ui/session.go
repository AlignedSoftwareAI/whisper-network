package ui

import (
	"errors"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/deweysasser/whisper-network/internal/server"
)

// loginView is the login page's data.
type loginView struct {
	Username string
	Next     string
	Error    string
}

// passwordView is the change-password page's data.
type passwordView struct {
	Forced bool
	Next   string
	Error  string
}

// handleLoginForm shows the login page, or skips it when the browser
// already has a session.
func (u *UI) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if sess, _, err := u.srv.SessionFromRequest(r); err == nil && sess != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	u.render(w, r, nil, http.StatusOK, "login", "Sign in", loginView{Next: next})
}

// busyMessage is shown when password hashing is saturated.
const busyMessage = "The server is busy. Try again in a few seconds."

// renderIfBusy renders page with the busy message and a 503 (plus
// Retry-After) when err is the server's busy refusal, and reports whether
// it did.
func (u *UI) renderIfBusy(w http.ResponseWriter, r *http.Request, rc *reqCtx, err error, page, title string,
	setError func(string) any) bool {
	secs, ok := server.Busy(err)
	if !ok {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	u.render(w, r, rc, http.StatusServiceUnavailable, page, title, setError(busyMessage))
	return true
}

// handleLogin checks credentials and starts a session. Failures say only
// that the username or password was wrong, and are logged as auth.denied
// without the submitted values.
func (u *UI) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	next := safeNext(r.PostFormValue("next"))
	username := strings.TrimSpace(r.PostFormValue("username"))
	view := loginView{Username: username, Next: next}

	if !u.logins.Allow(u.srv.ClientIP(r)) {
		u.logDenied(r, nil, "login rate limited")
		view.Error = "Too many sign-in attempts. Wait a minute and try again."
		w.Header().Set("Retry-After", "60")
		u.render(w, r, nil, http.StatusTooManyRequests, "login", "Sign in", view)
		return
	}
	user, err := u.srv.Login(r.Context(), username, r.PostFormValue("password"))
	if u.renderIfBusy(w, r, nil, err, "login", "Sign in", func(m string) any { view.Error = m; return view }) {
		return
	}
	if err == nil {
		// StartSession re-checks the verified password inside its
		// transaction: a reset or disable that landed since Login wins.
		_, err = u.srv.StartSession(w, r, user)
	}
	if errors.Is(err, server.ErrBadCredentials) {
		u.logDenied(r, nil, "bad credentials")
		if err := u.srv.Ops().LoginFailed(r.Context(), username, u.srv.ClientIP(r), "bad_credentials"); err != nil {
			u.fail(w, r, nil, err)
			return
		}
		view.Error = "Invalid username or password."
		u.render(w, r, nil, http.StatusUnauthorized, "login", "Sign in", view)
		return
	}
	if err != nil {
		u.fail(w, r, nil, err)
		return
	}
	if user.MustChangePassword {
		next = "/ui/password?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// handleLogout ends the session.
func (u *UI) handleLogout(w http.ResponseWriter, r *http.Request, _ *reqCtx) error {
	if err := u.srv.EndSession(w, r); err != nil {
		return err
	}
	u.redirect(w, r, "/ui/login")
	return nil
}

func (u *UI) handlePasswordForm(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	u.render(w, r, rc, http.StatusOK, "password", "Change password",
		passwordView{Forced: rc.User.MustChangePassword, Next: safeNext(r.URL.Query().Get("next"))})
	return nil
}

// handlePassword changes the user's password. The current password is
// required even on a forced change, so a stolen session cannot take over
// the account; checking it is rate limited per client IP (shared with
// sign-in) and per user. Every session of the user ends; a fresh one is
// started.
func (u *UI) handlePassword(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	view := passwordView{Forced: rc.User.MustChangePassword, Next: safeNext(r.PostFormValue("next"))}
	setErr := func(m string) any { view.Error = m; return view }
	current, pw := r.PostFormValue("current"), r.PostFormValue("password")
	verified := "" // the hash the current password was checked against
	switch {
	case len(pw) < minPasswordLen:
		view.Error = "The new password must be at least 12 characters."
	case pw != r.PostFormValue("confirm"):
		view.Error = "The new passwords do not match."
	case pw == current:
		view.Error = "The new password must differ from the current one."
	}
	if view.Error == "" {
		// Each check is a password guess: charge the caller's address
		// (shared with sign-in) and the user.
		if !u.logins.Allow(u.srv.ClientIP(r)) || !u.pwChecks.Allow("user:"+strconv.FormatInt(rc.User.ID, 10)) {
			u.logDenied(r, rc, "password check rate limited")
			view.Error = "Too many attempts. Wait a minute and try again."
			w.Header().Set("Retry-After", "60")
			u.render(w, r, rc, http.StatusTooManyRequests, "password", "Change password", view)
			return nil
		}
		checked, err := u.srv.Login(r.Context(), rc.User.Username, current)
		if err == nil {
			verified = checked.PasswordHash
		}
		switch {
		case u.renderIfBusy(w, r, rc, err, "password", "Change password", setErr):
			return nil
		case errors.Is(err, server.ErrBadCredentials):
			u.logDenied(r, rc, "wrong current password")
			view.Error = "The current password is incorrect."
		case err != nil:
			return err
		}
	}
	if view.Error != "" {
		u.render(w, r, rc, http.StatusBadRequest, "password", "Change password", view)
		return nil
	}
	// The change is made only if the checked hash is still current, so an
	// administrator's reset or disable since the check is not overwritten.
	err := u.srv.ChangePassword(r.Context(), rc.User, verified, pw)
	switch {
	case u.renderIfBusy(w, r, rc, err, "password", "Change password", setErr):
		return nil
	case errors.Is(err, server.ErrBadCredentials):
		u.logDenied(r, rc, "credentials changed during password change")
		view.Error = "Your password was changed or your account disabled meanwhile. Sign in again."
		u.render(w, r, rc, http.StatusBadRequest, "password", "Change password", view)
		return nil
	case err != nil:
		return err
	}
	if _, err := u.srv.StartSession(w, r, rc.User); err != nil {
		return err
	}
	http.Redirect(w, r, view.Next, http.StatusSeeOther)
	return nil
}

// safeNext returns next if it is a local UI path, else "/ui/". It refuses
// anything with a scheme or host (including protocol-relative "//host" and
// backslash tricks), paths that leave /ui/ once cleaned, and the login and
// logout pages themselves. It is the only gate against open redirects.
func safeNext(next string) string {
	const home = "/ui/"
	if next == "" || !strings.HasPrefix(next, home) || strings.ContainsAny(next, "\\\r\n\t") {
		return home
	}
	pu, err := url.Parse(next)
	if err != nil || pu.Scheme != "" || pu.Host != "" || pu.User != nil || pu.Opaque != "" {
		return home
	}
	clean := path.Clean(pu.Path)
	if !strings.HasPrefix(clean+"/", home) || strings.HasPrefix(clean, "//") {
		return home
	}
	if clean == "/ui/login" || clean == "/ui/logout" {
		return home
	}
	return next
}

// localPath extracts the path and query of an absolute URL (htmx's
// HX-Current-URL); the result still goes through safeNext.
func localPath(raw string) string {
	pu, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return pu.RequestURI()
}
