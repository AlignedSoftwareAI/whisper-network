package ui

import (
	"net/http"
	"strings"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/store"
)

// roleInfo is a grantable role with the words a form shows for it.
type roleInfo struct {
	Role  api.Role
	Label string // column or checkbox heading
	Help  string // one-line explanation, shown under the heading
}

// Roles a form may offer, per principal kind. process is never offered: only
// workers hold it, and they get it through approval. Clients are API
// programs, so they are not offered admin, worker_admin or queue_admin, which
// exist for the people who run the service.
var (
	userGlobalRoles = []roleInfo{
		{api.RoleAdmin, "Admin", "Everything, including users and queue weights"},
		{api.RoleWorkerAdmin, "Worker admin", "Approve, pause and reject any worker"},
		{api.RoleClientAdmin, "Client admin", "Create and revoke API clients"},
	}
	clientGlobalRoles = []roleInfo{
		{api.RoleClientAdmin, "Client admin", "Create and revoke API clients"},
	}
	userQueueRoles = []roleInfo{
		{api.RoleQueueAdmin, "Queue admin", "Approve workers for the queue; see and delete all its jobs"},
		{api.RoleSubmit, "Submit", "Create jobs; see and delete its own"},
		{api.RoleViewAll, "View all", "See every job and transcript"},
		{api.RoleDeleteAll, "Delete all", "Delete any job"},
	}
	clientQueueRoles = userQueueRoles[1:]
)

// userRoleNames lists every role a user may be given, for the per-user
// "add role" picker.
func userRoleNames() []api.Role {
	out := make([]api.Role, 0, len(userGlobalRoles)+len(userQueueRoles))
	for _, r := range append(append([]roleInfo{}, userGlobalRoles...), userQueueRoles...) {
		out = append(out, r.Role)
	}
	return out
}

// grantRule decides which grants a form's user may hand out:
// auth.CanGrantTo for existing principals and new users,
// auth.CanDelegateAtCreation for new clients.
type grantRule func(auth.Principal, []api.Grant) error

// grantCell is one checkbox of a grant matrix. Allowed is false when the
// person filling in the form may not hand out that grant under the form's
// grantRule; such cells render as "—".
type grantCell struct {
	Value   string // "<role> <scope>", parsed by parseGrantValues
	Allowed bool
	Checked bool // the principal being edited already holds this grant
}

// matrixRow is one queue's row of a grant matrix.
type matrixRow struct {
	Queue string
	Cells []grantCell
}

// globalGrant is one "*"-scoped role checkbox.
type globalGrant struct {
	roleInfo
	grantCell
}

// grantMatrix is the role picker on the create-user and create-client forms:
// global roles, then a table with a row per queue and a column per role.
type grantMatrix struct {
	Global  []globalGrant
	Columns []roleInfo
	Rows    []matrixRow
}

// Any reports whether the matrix has anything to show: a grant on offer, or
// one already held.
func (m grantMatrix) Any() bool {
	return len(m.Global) > 0 || len(m.Rows) > 0
}

// newGrantMatrix builds the picker for granting to a new principal of kind
// grantee under rule. Queues where granter can grant nothing are left out.
func newGrantMatrix(granter auth.Principal, grantee auth.PrincipalKind, queues []store.Queue, rule grantRule) grantMatrix {
	return grantMatrixFor(granter, grantee, queues, nil, rule)
}

// editGrantMatrix builds the picker pre-ticked with the grants an existing
// principal holds; what the editor may change follows auth.CanGrantTo. A
// held grant the editor may not give stays visible, ticked and disabled, so
// the editor sees it and saving leaves it alone.
func editGrantMatrix(granter auth.Principal, grantee auth.PrincipalKind, queues []store.Queue, held []api.Grant) grantMatrix {
	return grantMatrixFor(granter, grantee, queues, held, auth.CanGrantTo)
}

func grantMatrixFor(granter auth.Principal, grantee auth.PrincipalKind, queues []store.Queue, held []api.Grant, rule grantRule) grantMatrix {
	globals, cols := userGlobalRoles, userQueueRoles
	if grantee == auth.KindClient {
		globals, cols = clientGlobalRoles, clientQueueRoles
	}
	has := make(map[api.Grant]bool, len(held))
	for _, g := range held {
		has[g] = true
	}
	cell := func(g api.Grant) grantCell {
		return grantCell{Value: string(g.Role) + " " + g.Scope,
			Allowed: rule(granter, []api.Grant{g}) == nil, Checked: has[g]}
	}
	m := grantMatrix{Columns: cols}
	for _, ri := range globals {
		if c := cell(api.Grant{Role: ri.Role, Scope: store.ScopeAll}); c.Allowed || c.Checked {
			m.Global = append(m.Global, globalGrant{ri, c})
		}
	}
	for _, q := range queues {
		row := matrixRow{Queue: q.Name}
		show := false
		for _, ri := range cols {
			c := cell(api.Grant{Role: ri.Role, Scope: auth.QueueScope(q.Name)})
			show = show || c.Allowed || c.Checked
			row.Cells = append(row.Cells, c)
		}
		if show {
			m.Rows = append(m.Rows, row)
		}
	}
	return m
}

// grantChanges compares the grants submitted from an edit matrix with the
// grants a principal holds. Only grants the editor may give
// (auth.CanGrantTo, which also governs revocation) are ever removed; held grants outside the editor's authority are untouched
// whatever the form says. Additions are authorized by the caller.
func grantChanges(granter auth.Principal, held, submitted []api.Grant) (add, remove []api.Grant) {
	want := make(map[api.Grant]bool, len(submitted))
	for _, g := range submitted {
		want[g] = true
	}
	has := make(map[api.Grant]bool, len(held))
	for _, g := range held {
		has[g] = true
	}
	for g := range want {
		if !has[g] {
			add = append(add, g)
		}
	}
	for _, g := range held {
		if !want[g] && auth.CanGrantTo(granter, []api.Grant{g}) == nil {
			remove = append(remove, g)
		}
	}
	return add, remove
}

// parseGrantValues parses checkbox values back into grants. Authorization
// happens afterwards (auth.CanGrantTo or auth.CanDelegateAtCreation), so a
// forged value gains nothing.
func parseGrantValues(vals []string) ([]api.Grant, error) {
	out := make([]api.Grant, 0, len(vals))
	for _, v := range vals {
		role, scope, ok := strings.Cut(v, " ")
		if !ok {
			return nil, badRequest("Malformed grant.")
		}
		out = append(out, api.Grant{Role: api.Role(role), Scope: scope})
	}
	return out, nil
}

// grantForm reads the principal and grant of a grant or revoke form.
func grantForm(r *http.Request) (string, api.Grant) {
	return strings.TrimSpace(r.PostFormValue("principal")),
		api.Grant{Role: api.Role(r.PostFormValue("role")), Scope: strings.TrimSpace(r.PostFormValue("scope"))}
}

// handleGrant gives a principal a role (auth.CanGrantTo, as POST
// /admin/grants). It returns to the page named by "back".
func (u *UI) handleGrant(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	principal, g := grantForm(r)
	if err := auth.CanGrantTo(rc.P.Principal, []api.Grant{g}); err != nil {
		return u.deny(r, rc, err.Error())
	}
	if err := u.srv.Ops().Grant(r.Context(), rc.P.Actor(), principal, g); err != nil {
		return err
	}
	u.redirect(w, r, safeNext(r.PostFormValue("back")))
	return nil
}

// handleRevoke removes a role (auth.CanGrantTo, as DELETE /admin/grants).
func (u *UI) handleRevoke(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	principal, g := grantForm(r)
	if err := auth.CanGrantTo(rc.P.Principal, []api.Grant{g}); err != nil {
		return u.deny(r, rc, err.Error())
	}
	if err := u.srv.Ops().Revoke(r.Context(), rc.P.Actor(), principal, g); err != nil {
		return err
	}
	u.redirect(w, r, safeNext(r.PostFormValue("back")))
	return nil
}
