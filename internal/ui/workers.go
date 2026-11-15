package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/store"
)

// workersView is the workers page.
type workersView struct {
	Workers []workerView
}

// workerView is one worker card. CSRF is carried per card because the card
// is also rendered alone as an htmx fragment.
type workerView struct {
	CSRF         string
	ID           string
	State        api.WorkerState
	Facts        api.WorkerFacts
	RegisteredAt time.Time
	DecidedBy    string
	DecidedAt    *time.Time
	LastSeenAt   *time.Time
	Online       bool
	Gauges       *api.Gauges
	Queues       []workerQueue
	Stats        workStats

	// Controls, each shown only when the principal may use it.
	CanApprove          bool     // lifecycle half (worker_admin)
	ApproveQueues       []string // queues the principal may grant process on
	CanPause, CanResume bool
	CanReject           bool
	AddQueues           []string // further queues grantable to an active worker
}

// workStats is a worker's counters with the figures the page derives
// from them.
type workStats struct {
	api.WorkerStats
	TasksDone   int64
	AudioHours  float64
	RTF         float64 // audio seconds per compute second; 0 if none
	FailurePct  float64 // failed and expired leases, of counted outcomes
	ComputeTime time.Duration
}

func newWorkStats(s api.WorkerStats) workStats {
	return workStats{WorkerStats: s, TasksDone: s.TasksDone(), AudioHours: s.AudioS / 3600,
		RTF: s.RealtimeFactor(), FailurePct: 100 * s.FailureRate(),
		ComputeTime: time.Duration(s.ComputeS * float64(time.Second))}
}

// workerQueue is a queue a worker may process.
type workerQueue struct {
	Name      string
	CanRevoke bool
}

// handleWorkers lists workers, pending first. Who may list and which
// workers they see mirror GET /admin/workers: a queue_admin sees every
// worker (to approve them for its queues), a worker_admin only those in
// scope.
func (u *UI) handleWorkers(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	if !mayListWorkers(rc.P) {
		return u.deny(r, rc, "worker_admin or queue_admin required")
	}
	ctx := r.Context()
	ws, err := u.srv.Store().ListWorkers(ctx)
	if err != nil {
		return err
	}
	queues, err := u.queueNames(ctx)
	if err != nil {
		return err
	}
	queueAdmin := holdsRole(rc.P, api.RoleQueueAdmin)
	var v workersView
	for i := range ws {
		wk := &ws[i]
		if !queueAdmin && !rc.can(auth.ActViewWorker, auth.Resource{WorkerID: wk.ID}) {
			continue
		}
		wv, err := u.workerView(ctx, rc, wk, queues)
		if err != nil {
			return err
		}
		v.Workers = append(v.Workers, *wv)
	}
	slices.SortStableFunc(v.Workers, func(a, b workerView) int { return stateRank(a.State) - stateRank(b.State) })
	u.render(w, r, rc, http.StatusOK, "workers", "Workers", v)
	return nil
}

// stateRank orders the workers page: pending first, rejected last.
func stateRank(s api.WorkerState) int {
	switch s {
	case api.WorkerPending:
		return 0
	case api.WorkerActive:
		return 1
	case api.WorkerPaused:
		return 2
	}
	return 3
}

func (u *UI) workerView(ctx context.Context, rc *reqCtx, wk *store.Worker, queues []string) (*workerView, error) {
	grants, err := u.srv.Ops().WireGrants(ctx, wk.Principal())
	if err != nil {
		return nil, err
	}
	now := u.srv.Now()
	res := auth.Resource{WorkerID: wk.ID}
	v := &workerView{CSRF: rc.CSRF, ID: wk.ID, State: wk.State, Facts: wk.Facts, RegisteredAt: wk.RegisteredAt,
		DecidedBy: wk.DecidedBy, DecidedAt: wk.DecidedAt, LastSeenAt: wk.LastSeenAt, Online: isOnline(wk, now),
		Stats: newWorkStats(wk.Stats)}
	if len(wk.Gauges) > 0 {
		var g api.Gauges
		if json.Unmarshal(wk.Gauges, &g) == nil {
			v.Gauges = &g
		}
	}
	held := map[string]bool{}
	for _, g := range grants {
		if g.Role != api.RoleProcess {
			continue
		}
		name := strings.TrimPrefix(g.Scope, "queue:")
		held[name] = true
		v.Queues = append(v.Queues, workerQueue{Name: name,
			CanRevoke: auth.CanGrantTo(rc.P.Principal, []api.Grant{g}) == nil})
	}
	var grantable []string
	for _, q := range queues {
		if rc.can(auth.ActGrantProcess, auth.Resource{Queue: q}) {
			grantable = append(grantable, q)
		}
	}
	switch wk.State {
	case api.WorkerPending:
		v.CanApprove = rc.can(auth.ActApproveWorker, res)
		if v.CanApprove {
			v.ApproveQueues = grantable
		}
	case api.WorkerActive, api.WorkerPaused:
		for _, q := range grantable {
			if !held[q] {
				v.AddQueues = append(v.AddQueues, q)
			}
		}
	}
	v.CanPause = wk.State == api.WorkerActive && rc.can(auth.ActPauseWorker, res)
	v.CanResume = wk.State == api.WorkerPaused && rc.can(auth.ActResumeWorker, res)
	v.CanReject = wk.State != api.WorkerRejected && rc.can(auth.ActRejectWorker, res)
	return v, nil
}

// handleApproveWorker approves a worker for the chosen queues: worker_admin
// over the worker and queue_admin on every chosen queue (auth.CanApproveWorker,
// as POST /admin/workers/{id}/approve).
func (u *UI) handleApproveWorker(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		return badRequest("Malformed form.")
	}
	queues := r.PostForm["queue"]
	if err := auth.CanApproveWorker(rc.P.Principal, id, queues); err != nil {
		return u.deny(r, rc, err.Error())
	}
	if err := u.srv.Ops().ApproveWorker(r.Context(), rc.P.Actor(), id, queues); err != nil {
		return err
	}
	return u.workerDone(w, r, rc, id)
}

// handleWorkerState pauses, resumes or rejects a worker (worker_admin, as
// POST /admin/workers/{id}/{pause,resume,reject}).
func (u *UI) handleWorkerState(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	id := r.PathValue("id")
	var act auth.Action
	var state api.WorkerState
	switch r.PathValue("verb") {
	case "pause":
		act, state = auth.ActPauseWorker, api.WorkerPaused
	case "resume":
		act, state = auth.ActResumeWorker, api.WorkerActive
	case "reject":
		act, state = auth.ActRejectWorker, api.WorkerRejected
	default:
		return errNotFound
	}
	if !rc.can(act, auth.Resource{WorkerID: id}) {
		return u.deny(r, rc, "worker_admin required")
	}
	if err := u.srv.Ops().SetWorkerState(r.Context(), rc.P.Actor(), id, state); err != nil {
		return err
	}
	return u.workerDone(w, r, rc, id)
}

// workerDone answers a worker action: the refreshed card for htmx, or a
// redirect back to the list.
func (u *UI) workerDone(w http.ResponseWriter, r *http.Request, rc *reqCtx, id string) error {
	if !isHTMX(r) {
		http.Redirect(w, r, "/ui/workers#w-"+id, http.StatusSeeOther)
		return nil
	}
	wk, err := u.srv.Store().WorkerByID(r.Context(), id)
	if err != nil {
		return err
	}
	queues, err := u.queueNames(r.Context())
	if err != nil {
		return err
	}
	v, err := u.workerView(r.Context(), rc, wk, queues)
	if err != nil {
		return err
	}
	u.renderFragment(w, http.StatusOK, "workers", "worker", v)
	return nil
}

// queueNames returns every queue name, sorted.
func (u *UI) queueNames(ctx context.Context) ([]string, error) {
	qs, err := u.srv.Store().ListQueues(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(qs))
	for i, q := range qs {
		out[i] = q.Name
	}
	return out, nil
}
