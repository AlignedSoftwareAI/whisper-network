package ui

import (
	"context"
	"errors"
	"net/http"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/store"
)

// clientsView is the clients page. Created carries the new client's token
// only in the response to the create form: the token is shown once and is
// not stored anywhere it could be shown again.
type clientsView struct {
	CSRF    string
	Clients []clientRow
	Grants  grantMatrix
	Created *api.Client
}

// clientRow is one client in the list. CanRevoke follows
// auth.CanRevokeClient: only clients the viewer could have created.
type clientRow struct {
	api.Client
	CanRevoke bool
}

// handleClients lists API clients (client_admin, as GET /admin/clients).
func (u *UI) handleClients(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	if !rc.can(auth.ActCreateClient, auth.Resource{}) {
		return u.deny(r, rc, "client_admin required")
	}
	v, err := u.clientsView(r.Context(), rc)
	if err != nil {
		return err
	}
	u.render(w, r, rc, http.StatusOK, "clients", "Clients", v)
	return nil
}

func (u *UI) clientsView(ctx context.Context, rc *reqCtx) (*clientsView, error) {
	cs, err := u.srv.Store().ListClients(ctx)
	if err != nil {
		return nil, err
	}
	qs, err := u.srv.Store().ListQueues(ctx)
	if err != nil {
		return nil, err
	}
	v := &clientsView{CSRF: rc.CSRF,
		Grants: newGrantMatrix(rc.P.Principal, auth.KindClient, qs, auth.CanDelegateAtCreation)}
	for i := range cs {
		c, err := u.srv.Ops().ClientAPI(ctx, &cs[i])
		if err != nil {
			return nil, err
		}
		v.Clients = append(v.Clients, clientRow{Client: *c,
			CanRevoke: auth.CanRevokeClient(rc.P.Principal, c.ID, c.Grants) == nil})
	}
	return v, nil
}

// handleCreateClient mints a client token (client_admin, with grants bounded
// by auth.CanDelegateAtCreation, as POST /client).
func (u *UI) handleCreateClient(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	if !rc.can(auth.ActCreateClient, auth.Resource{}) {
		return u.deny(r, rc, "client_admin required")
	}
	if err := r.ParseForm(); err != nil {
		return badRequest("Malformed form.")
	}
	grants, err := parseGrantValues(r.PostForm["grant"])
	if err != nil {
		return err
	}
	if err := auth.CanDelegateAtCreation(rc.P.Principal, grants); err != nil {
		return u.deny(r, rc, err.Error())
	}
	created, err := u.srv.Ops().CreateClient(r.Context(), rc.P.Actor(), r.PostFormValue("name"), grants)
	if err != nil {
		return err
	}
	v, err := u.clientsView(r.Context(), rc)
	if err != nil {
		return err
	}
	v.Created = created
	u.render(w, r, rc, http.StatusCreated, "clients", "Clients", v)
	return nil
}

// handleRevokeClient revokes a client's token (auth.CanRevokeClient, as
// DELETE /client/{id}).
func (u *UI) handleRevokeClient(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	id := r.PathValue("id")
	if !rc.can(auth.ActRevokeClient, auth.Resource{ClientID: id}) {
		return u.deny(r, rc, "client_admin required")
	}
	c, err := u.srv.Store().ClientByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	ac, err := u.srv.Ops().ClientAPI(r.Context(), c)
	if err != nil {
		return err
	}
	if err := auth.CanRevokeClient(rc.P.Principal, id, ac.Grants); err != nil {
		return u.deny(r, rc, err.Error())
	}
	if err := u.srv.Ops().RevokeClient(r.Context(), rc.P.Actor(), id); err != nil {
		return err
	}
	u.redirect(w, r, "/ui/clients")
	return nil
}

// clientView is one client's page (#60): its details, recent activity
// and, unless it is revoked, its permissions as an editable matrix.
type clientView struct {
	Client         api.Client
	CreatedBy      ref
	Activity       []auditRow // the client's latest audit entries as actor
	Grants         grantMatrix
	CanEdit        bool // not revoked
	Saved          bool
	Added, Removed int
}

// clientActivity is how many of a client's audit entries its page shows.
const clientActivity = 20

// viewableClient loads the client in the path for a client admin, revoked
// or not.
func (u *UI) viewableClient(r *http.Request, rc *reqCtx) (*store.Client, *api.Client, error) {
	id := r.PathValue("id")
	if !rc.can(auth.ActRevokeClient, auth.Resource{ClientID: id}) {
		return nil, nil, u.deny(r, rc, "client_admin required")
	}
	c, err := u.srv.Store().ClientByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, errNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	ac, err := u.srv.Ops().ClientAPI(r.Context(), c)
	if err != nil {
		return nil, nil, err
	}
	return c, ac, nil
}

// editableClient loads a client for editing: client_admin, and not revoked
// (a revoked token can never be used again, so its grants are moot). Which
// grants the editor may change is auth.CanGrantTo's business, not this
// gate's.
func (u *UI) editableClient(r *http.Request, rc *reqCtx) (*store.Client, *api.Client, error) {
	c, ac, err := u.viewableClient(r, rc)
	if err != nil {
		return nil, nil, err
	}
	if ac.RevokedAt != nil {
		return nil, nil, badRequest("This client is revoked; its permissions can no longer be changed.")
	}
	return c, ac, nil
}

func (u *UI) clientPageView(ctx context.Context, rc *reqCtx, sc *store.Client, c *api.Client) (*clientView, error) {
	v := &clientView{Client: *c, CanEdit: c.RevokedAt == nil}
	es, err := u.srv.Store().QueryAudit(ctx, store.AuditFilter{Actor: sc.Principal(), Limit: clientActivity})
	if err != nil {
		return nil, err
	}
	var ids labelIDs
	ids.principal(sc.CreatedBy)
	for _, e := range es {
		ids.principal(e.Target)
	}
	lb, err := u.labels(ctx, rc, ids)
	if err != nil {
		return nil, err
	}
	v.CreatedBy = lb.principal(sc.CreatedBy)
	for _, e := range es {
		row := auditRow{AuditEntry: e}
		if e.Target != "" {
			row.TargetRef = lb.principal(e.Target)
		}
		v.Activity = append(v.Activity, row)
	}
	if v.CanEdit {
		qs, err := u.srv.Store().ListQueues(ctx)
		if err != nil {
			return nil, err
		}
		v.Grants = editGrantMatrix(rc.P.Principal, auth.KindClient, qs, c.Grants)
	}
	return v, nil
}

// handleClient shows a client: details, activity and editable permissions.
func (u *UI) handleClient(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	sc, c, err := u.viewableClient(r, rc)
	if err != nil {
		return err
	}
	v, err := u.clientPageView(r.Context(), rc, sc, c)
	if err != nil {
		return err
	}
	u.render(w, r, rc, http.StatusOK, "client", "Client "+c.Name, v)
	return nil
}

// handleUpdateClient applies the difference between the submitted matrix and
// the client's current grants. Every addition is checked with
// auth.CanGrantTo before anything changes, so a forged form changes nothing;
// removals are limited to grants the editor could give (the same rule).
// The whole difference is applied and audited in one transaction by
// server.Ops.UpdateGrants.
func (u *UI) handleUpdateClient(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	_, c, err := u.editableClient(r, rc)
	if err != nil {
		return err
	}
	if err := r.ParseForm(); err != nil {
		return badRequest("Malformed form.")
	}
	submitted, err := parseGrantValues(r.PostForm["grant"])
	if err != nil {
		return err
	}
	add, remove := grantChanges(rc.P.Principal, c.Grants, submitted)
	if err := auth.CanGrantTo(rc.P.Principal, add); err != nil {
		return u.deny(r, rc, err.Error())
	}
	if len(add) > 0 || len(remove) > 0 {
		if err := u.srv.Ops().UpdateGrants(r.Context(), rc.P.Actor(), "client:"+c.ID, add, remove); err != nil {
			return err
		}
	}
	stored, err := u.srv.Store().ClientByID(r.Context(), c.ID)
	if err != nil {
		return err
	}
	if c, err = u.srv.Ops().ClientAPI(r.Context(), stored); err != nil {
		return err
	}
	v, err := u.clientPageView(r.Context(), rc, stored, c)
	if err != nil {
		return err
	}
	v.Saved, v.Added, v.Removed = true, len(add), len(remove)
	u.render(w, r, rc, http.StatusOK, "client", "Client "+c.Name, v)
	return nil
}
