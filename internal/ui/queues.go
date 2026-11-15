package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
)

// queuesView is the queue list.
type queuesView struct {
	Queues    []queueItem
	CanCreate bool
}

type queueItem struct {
	store.Queue
	SeesJobs bool
}

// queueView is one queue: settings, jobs and grants.
type queueView struct {
	Queue                                  store.Queue
	CanEditDesc, CanEditMax, CanEditWeight bool

	Visibility auth.Visibility
	SeesJobs   bool
	OwnOnly    bool // SeesOwn: the list holds the principal's jobs only
	Jobs       []jobRow
	State      string // the state filter in force
	States     []api.JobState
	NextPage   string // URL of the next page, if any

	CanManageGrants bool
	Grants          []grantRow
	GrantRoles      []api.Role // roles the principal may hand out here
	Back            string
}

// grantRow is one principal's role on a scope, with a revoke control.
type grantRow struct {
	Principal string // wire form: user:<name>, client:<id>, worker:<id>
	Role      api.Role
	Scope     string
	CanRevoke bool
}

func (u *UI) handleQueues(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	qs, err := u.srv.Store().ListQueues(r.Context())
	if err != nil {
		return err
	}
	v := queuesView{CanCreate: rc.can(auth.ActCreateQueue, auth.Resource{})}
	for _, q := range qs {
		v.Queues = append(v.Queues, queueItem{Queue: q,
			SeesJobs: auth.VisibilityOn(rc.P.Principal, q.Name) != auth.SeesNone})
	}
	u.render(w, r, rc, http.StatusOK, "queues", "Queues", v)
	return nil
}

// handleCreateQueue creates a queue (admin, as POST /admin/queues).
func (u *UI) handleCreateQueue(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	if !rc.can(auth.ActCreateQueue, auth.Resource{}) {
		return u.deny(r, rc, "admin required")
	}
	q := api.Queue{Name: strings.TrimSpace(r.PostFormValue("name")), Description: r.PostFormValue("description")}
	var err error
	if q.Weight, err = optionalInt(r.PostFormValue("weight")); err != nil {
		return badRequest("Weight must be a whole number from 1 to 10000.")
	}
	if q.MaxUploadBytes, err = optionalBytes(r.PostFormValue("max_upload")); err != nil {
		return badRequest("Max upload must be a size such as 500MiB or 2GiB.")
	}
	created, err := u.srv.Ops().CreateQueue(r.Context(), rc.P.Actor(), q)
	if err != nil {
		return err
	}
	http.Redirect(w, r, "/ui/queues/"+url.PathEscape(created.Name), http.StatusSeeOther)
	return nil
}

// handleQueue shows a queue. Anyone signed in may see its settings (as GET
// /admin/queues); jobs follow auth.VisibilityOn (as GET /queue/{q}); grants
// are shown to those who may manage them.
func (u *UI) handleQueue(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	ctx := r.Context()
	q, err := u.srv.Store().QueueByName(ctx, r.PathValue("q"))
	if err != nil {
		return err
	}
	res := auth.Resource{Queue: q.Name}
	v := queueView{Queue: *q,
		CanEditDesc:   rc.can(auth.ActPatchQueueDescription, res),
		CanEditMax:    rc.can(auth.ActPatchQueueMaxUpload, res),
		CanEditWeight: rc.can(auth.ActSetQueueWeight, res),
		Visibility:    auth.VisibilityOn(rc.P.Principal, q.Name),
		States:        []api.JobState{api.JobPreparing, api.JobQueued, api.JobRunning, api.JobDone, api.JobFailed},
		Back:          "/ui/queues/" + url.PathEscape(q.Name),
	}
	v.SeesJobs = v.Visibility != auth.SeesNone
	v.OwnOnly = v.Visibility == auth.SeesOwn
	if v.SeesJobs {
		if err := u.queueJobs(r, rc, q, &v); err != nil {
			return err
		}
	}
	if rc.can(auth.ActGrantQueueRole, res) {
		v.CanManageGrants = true
		if v.Grants, err = u.queueGrants(ctx, rc, q); err != nil {
			return err
		}
		for _, role := range []api.Role{api.RoleQueueAdmin, api.RoleSubmit, api.RoleViewAll, api.RoleReadAll, api.RoleDeleteAll} {
			if auth.CanGrantTo(rc.P.Principal, []api.Grant{{Role: role, Scope: auth.QueueScope(q.Name)}}) == nil {
				v.GrantRoles = append(v.GrantRoles, role)
			}
		}
	}
	u.render(w, r, rc, http.StatusOK, "queue", "Queue "+q.Name, v)
	return nil
}

// queueJobs fills the job list: everything for SeesAll, the principal's own
// jobs for SeesOwn, filtered by state and paged by cursor.
func (u *UI) queueJobs(r *http.Request, rc *reqCtx, q *store.Queue, v *queueView) error {
	qv := r.URL.Query()
	f := store.JobFilter{QueueID: q.ID, Limit: 50, Cursor: qv.Get("cursor")}
	if v.Visibility == auth.SeesOwn {
		f.ClientID = rc.P.ID
	}
	if s := qv.Get("state"); s != "" {
		if !validJobState(api.JobState(s)) {
			return badRequest("Unknown job state.")
		}
		f.States = []api.JobState{api.JobState(s)}
		v.State = s
	}
	jobs, next, err := u.srv.Store().ListJobs(r.Context(), f)
	if errors.Is(err, store.ErrInvalid) {
		return badRequest("Bad page cursor.")
	}
	if err != nil {
		return err
	}
	if v.Jobs, err = u.queueJobRows(r.Context(), rc, q.Name, jobs); err != nil {
		return err
	}
	if next != "" {
		p := url.Values{"cursor": {next}}
		if v.State != "" {
			p.Set("state", v.State)
		}
		v.NextPage = v.Back + "?" + p.Encode()
	}
	return nil
}

func validJobState(s api.JobState) bool {
	switch s {
	case api.JobPreparing, api.JobQueued, api.JobRunning, api.JobDone, api.JobFailed:
		return true
	}
	return false
}

// queueGrants lists who holds which role on q, in wire form.
func (u *UI) queueGrants(ctx context.Context, rc *reqCtx, q *store.Queue) ([]grantRow, error) {
	st := u.srv.Store()
	users, err := st.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(users))
	for i := range users {
		names[users[i].Principal()] = "user:" + users[i].Username
	}
	var out []grantRow
	scope := auth.QueueScope(q.Name)
	for _, role := range []api.Role{api.RoleQueueAdmin, api.RoleSubmit, api.RoleViewAll, api.RoleReadAll, api.RoleDeleteAll, api.RoleProcess} {
		ps, err := st.PrincipalsWithRole(ctx, role, store.QueueScope(q.ID))
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			if n, ok := names[p]; ok {
				p = n
			}
			out = append(out, grantRow{Principal: p, Role: role, Scope: scope,
				CanRevoke: auth.CanGrantTo(rc.P.Principal, []api.Grant{{Role: role, Scope: scope}}) == nil})
		}
	}
	return out, nil
}

// handlePatchQueue edits a queue. Only fields that changed are applied, and
// each one is authorized exactly as PATCH /queue/{q} does: weight is admin
// only, description and upload cap need queue_admin.
func (u *UI) handlePatchQueue(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	q, err := u.srv.Store().QueueByName(r.Context(), r.PathValue("q"))
	if err != nil {
		return err
	}
	if err := r.ParseForm(); err != nil {
		return badRequest("Malformed form.")
	}
	var patch api.QueuePatch
	if _, ok := r.PostForm["description"]; ok {
		if d := r.PostFormValue("description"); d != q.Description {
			patch.Description = &d
		}
	}
	if _, ok := r.PostForm["max_upload"]; ok {
		n, err := optionalBytes(r.PostFormValue("max_upload"))
		if err != nil {
			return badRequest("Max upload must be a size such as 500MiB or 2GiB.")
		}
		if n != q.MaxUploadBytes {
			patch.MaxUploadBytes = &n
		}
	}
	if _, ok := r.PostForm["weight"]; ok {
		n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("weight")))
		if err != nil {
			return badRequest("Weight must be a whole number from 1 to 10000.")
		}
		if n != q.Weight {
			patch.Weight = &n
		}
	}
	res := auth.Resource{Queue: q.Name}
	checks := []struct {
		set bool
		act auth.Action
		why string
	}{
		{patch.Weight != nil, auth.ActSetQueueWeight, "only admin may change a queue's weight"},
		{patch.Description != nil, auth.ActPatchQueueDescription, "queue_admin required"},
		{patch.MaxUploadBytes != nil, auth.ActPatchQueueMaxUpload, "queue_admin required"},
	}
	for _, c := range checks {
		if c.set && !rc.can(c.act, res) {
			return u.deny(r, rc, c.why)
		}
	}
	if patch != (api.QueuePatch{}) {
		if _, err := u.srv.Ops().PatchQueue(r.Context(), rc.P.Actor(), q, patch); err != nil {
			return err
		}
	}
	http.Redirect(w, r, "/ui/queues/"+url.PathEscape(q.Name), http.StatusSeeOther)
	return nil
}

func optionalInt(s string) (int, error) {
	if s = strings.TrimSpace(s); s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

// optionalBytes parses a size; empty means 0 (the server default).
func optionalBytes(s string) (int64, error) {
	if s = strings.TrimSpace(s); s == "" || s == "0" {
		return 0, nil
	}
	return server.ParseByteSize(s)
}
