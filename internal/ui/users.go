package ui

import (
	"context"
	"net/http"
	"strings"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
)

// usersView is the users page. Created is set only in the response to the
// create form, so a generated password is shown exactly once.
type usersView struct {
	CSRF      string
	Self      string
	Users     []userRow
	Grants    grantMatrix
	UserRoles []api.Role
	Created   *api.User
}

// userRow is one user with revocable grants.
type userRow struct {
	api.User
	Grants []grantRow
}

// handleUsers lists users (admin, as GET /admin/users).
func (u *UI) handleUsers(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	if !rc.can(auth.ActCreateUser, auth.Resource{}) {
		return u.deny(r, rc, "admin required")
	}
	v, err := u.usersView(r.Context(), rc)
	if err != nil {
		return err
	}
	u.render(w, r, rc, http.StatusOK, "users", "Users", v)
	return nil
}

func (u *UI) usersView(ctx context.Context, rc *reqCtx) (*usersView, error) {
	us, err := u.srv.Store().ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	qs, err := u.srv.Store().ListQueues(ctx)
	if err != nil {
		return nil, err
	}
	v := &usersView{CSRF: rc.CSRF, Self: rc.User.Username, Grants: newGrantMatrix(rc.P.Principal, auth.KindUser, qs, auth.CanGrantTo),
		UserRoles: userRoleNames()}
	for i := range us {
		au, err := u.srv.Ops().UserAPI(ctx, &us[i])
		if err != nil {
			return nil, err
		}
		row := userRow{User: *au}
		for _, g := range au.Grants {
			row.Grants = append(row.Grants, grantRow{Principal: "user:" + au.Username, Role: g.Role, Scope: g.Scope,
				CanRevoke: auth.CanGrantTo(rc.P.Principal, []api.Grant{g}) == nil})
		}
		v.Users = append(v.Users, row)
	}
	return v, nil
}

// handleCreateUser creates a user with grants (admin, plus auth.CanGrantTo, as
// POST /admin/users). A generated password is rendered once in this
// response and nowhere else.
func (u *UI) handleCreateUser(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	if !rc.can(auth.ActCreateUser, auth.Resource{}) {
		return u.deny(r, rc, "admin required")
	}
	if err := r.ParseForm(); err != nil {
		return badRequest("Malformed form.")
	}
	grants, err := parseGrantValues(r.PostForm["grant"])
	if err != nil {
		return err
	}
	if err := auth.CanGrantTo(rc.P.Principal, grants); err != nil {
		return u.deny(r, rc, err.Error())
	}
	req := api.CreateUserRequest{Username: strings.TrimSpace(r.PostFormValue("username")),
		Password: r.PostFormValue("password"), Grants: grants}
	if req.Password != "" && len(req.Password) < minPasswordLen {
		return badRequest("Passwords must be at least 12 characters; leave it empty to generate one.")
	}
	created, err := u.srv.Ops().CreateUser(r.Context(), rc.P.Actor(), req)
	if err != nil {
		return err
	}
	v, err := u.usersView(r.Context(), rc)
	if err != nil {
		return err
	}
	v.Created = created
	u.render(w, r, rc, http.StatusCreated, "users", "Users", v)
	return nil
}

// handleDisableUser disables a user and ends their sessions (admin).
// Disabling yourself is refused so the last admin cannot lock everyone out
// by accident.
func (u *UI) handleDisableUser(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	if !rc.can(auth.ActCreateUser, auth.Resource{}) {
		return u.deny(r, rc, "admin required")
	}
	name := r.PathValue("name")
	if name == rc.User.Username {
		return badRequest("You cannot disable your own account.")
	}
	if err := u.srv.Ops().DisableUser(r.Context(), rc.P.Actor(), name); err != nil {
		return err
	}
	u.redirect(w, r, "/ui/users")
	return nil
}
