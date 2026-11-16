package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/store"
)

// jobView is the job detail page.
type jobView struct {
	CSRF string
	Job  api.Job
	// Name is the job's label (else source) and Ref the job as a ref, for
	// the header (#58).
	Name string
	Ref  ref
	// Error is the job's error with the workers it names as refs (#57).
	Error []segment
	// Submitter is who created the job (#60): a client is named and linked
	// for those who may view clients (else "a client"), a user by name.
	Submitter *ref
	Owner     string
	Tasks     []taskRow
	Running   []runningRow
	// Waiting are the tasks not running or finished (#54): pending ones
	// with how long they have waited and why, failed and cancelled ones.
	Waiting     []waitingRow
	CanDelete   bool
	CanDownload bool // a done job the viewer may read (auth.ActDownloadTranscript)
	// CanPartial offers the partial transcript (#66): a job that is not
	// done, has finished transcribe tasks, and that the viewer may read.
	CanPartial bool
	// CanRetry offers Retry (#56): a failed job with its media, and a viewer
	// who may retry it (auth.ActRetryJob).
	CanRetry bool
}

// taskRow is one finished task. Running tasks are runningRows, the rest
// waitingRows.
type taskRow struct {
	Seq           int
	Kind          api.TaskKind
	StartS, EndS  float64
	Worker        ref
	EngineVersion string
	ComputeS      float64
}

// waitingRow is a task that is pending, failed or cancelled.
type waitingRow struct {
	Seq          int
	Kind         api.TaskKind
	State        api.TaskState
	StartS, EndS float64
	Waiting      time.Duration // pending: since it last became pending
	Attempts     int
	// LastError is the latest failure reason, with the workers it names;
	// LastWorker the worker of the latest counted failure, if any.
	LastError  []segment
	LastWorker *ref
	Reasons    []string // pending: why no eligible worker takes it now
}

// visibleJob loads the job in the path for a principal that may see it. A
// job the principal cannot see is 404, never 403, exactly as the API
// (DESIGN.md §5.3).
func (u *UI) visibleJob(r *http.Request, rc *reqCtx) (*store.Job, auth.Resource, error) {
	j, err := u.srv.Store().Job(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		return nil, auth.Resource{}, errNotFound
	}
	if err != nil {
		return nil, auth.Resource{}, err
	}
	res := auth.Resource{Queue: j.QueueName, OwnerID: j.ClientID}
	if !rc.can(auth.ActViewJob, res) {
		return nil, res, u.hide(r, rc, "job not visible")
	}
	return j, res, nil
}

func (u *UI) handleJob(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	ctx := r.Context()
	j, res, err := u.visibleJob(r, rc)
	if err != nil {
		return err
	}
	st := u.srv.Store()
	outs, err := st.JobOutputs(ctx, j.ID)
	if err != nil {
		return err
	}
	tasks, err := st.JobTasks(ctx, j.ID)
	if err != nil {
		return err
	}
	canRead := rc.can(auth.ActDownloadTranscript, res)
	v := jobView{CSRF: rc.CSRF, Job: u.srv.JobDoc(ctx, rc.P.Principal, j), Owner: j.ClientID,
		CanDelete:   rc.can(auth.ActDeleteJob, res),
		CanDownload: j.State == api.JobDone && canRead,
		CanPartial:  j.State != api.JobDone && canRead && len(outs.Transcribe) > 0,
		CanRetry:    j.Retryable() && rc.can(auth.ActRetryJob, res)}

	var ids labelIDs
	ids.job(j.ID)
	if sub := v.Job.SubmittedBy; sub != nil && sub.Kind == "client" {
		ids.client(sub.ID)
	}
	var failed []string // every worker a failure reason may name
	for _, t := range tasks {
		ids.worker(t.WorkerID)
		ids.worker(t.FailedOn...)
		failed = append(failed, t.FailedOn...)
		failed = append(failed, t.WorkerID)
	}
	lb, err := u.labels(ctx, rc, ids)
	if err != nil {
		return err
	}
	v.Ref = lb.job(j.ID)
	v.Name = v.Ref.Name
	if v.Job.Error != nil {
		v.Error = lb.withWorkers(*v.Job.Error, j.QueueName, failed)
	}
	if sub := v.Job.SubmittedBy; sub != nil && sub.Kind == "client" {
		c := lb.client(sub.ID)
		v.Submitter = &c
	}

	if p := outs.Prepare; p != nil {
		v.Tasks = append(v.Tasks, taskRow{Seq: p.Seq, Kind: api.KindPrepare, Worker: lb.worker(p.WorkerID, j.QueueName),
			EngineVersion: p.EngineVersion, ComputeS: float64(p.ComputeMS) / 1000})
	}
	for _, t := range outs.Transcribe {
		v.Tasks = append(v.Tasks, taskRow{Seq: t.Seq, Kind: api.KindTranscribe,
			StartS: samplesToSeconds(t.StartSample), EndS: samplesToSeconds(t.EndSample),
			Worker: lb.worker(t.WorkerID, j.QueueName), EngineVersion: t.EngineVersion,
			ComputeS: float64(t.ComputeMS) / 1000})
	}
	if v.Waiting, err = u.waitingRows(ctx, lb, j, tasks, failed); err != nil {
		return err
	}
	if v.Running, err = u.runningRows(ctx, rc, j); err != nil {
		return err
	}
	u.render(w, r, rc, http.StatusOK, "job", "Job", v)
	return nil
}

// waitingRows builds the rows of the job's pending, failed and cancelled
// tasks. The queue's eligible workers are read only if a task is pending.
func (u *UI) waitingRows(ctx context.Context, lb *labeler, j *store.Job, tasks []store.TaskInfo,
	failed []string) ([]waitingRow, error) {
	var fleet *fleetView
	now := u.srv.Now()
	var out []waitingRow
	for _, t := range tasks {
		if t.State == api.TaskDone || t.State == api.TaskLeased {
			continue
		}
		row := waitingRow{Seq: t.Seq, Kind: t.Kind, State: t.State, Attempts: t.Attempts}
		if t.Kind == api.KindTranscribe {
			row.StartS, row.EndS = samplesToSeconds(t.StartSample), samplesToSeconds(t.EndSample)
		}
		if t.Error != "" {
			row.LastError = lb.withWorkers(t.Error, j.QueueName, failed)
		}
		if t.LastFailedOn != "" {
			wr := lb.worker(t.LastFailedOn, j.QueueName)
			row.LastWorker = &wr
		}
		if t.State == api.TaskPending {
			if t.PendingSince != nil {
				row.Waiting = now.Sub(*t.PendingSince)
			}
			if fleet == nil {
				var err error
				if fleet, err = u.queueFleet(ctx, j.QueueName, j.QueueID); err != nil {
					return nil, err
				}
			}
			row.Reasons = fleet.waitingReasons(t.Kind, j.Model, t.FailedOn)
		}
		out = append(out, row)
	}
	return out, nil
}

// handleDeleteJob deletes a job: the owner, delete_all or queue_admin on its
// queue (auth.ActDeleteJob), as DELETE /job/{id}.
func (u *UI) handleDeleteJob(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	j, res, err := u.visibleJob(r, rc)
	if err != nil {
		return err
	}
	if !rc.can(auth.ActDeleteJob, res) {
		return u.deny(r, rc, "not allowed on this job")
	}
	if err := u.srv.DeleteJob(r.Context(), rc.P, j.ID); err != nil {
		return err
	}
	u.redirect(w, r, "/ui/queues/"+url.PathEscape(j.QueueName))
	return nil
}

// handleRetryJob retries a failed job: its owner, queue_admin on its queue
// or admin (auth.ActRetryJob), as POST /job/{id}/retry.
func (u *UI) handleRetryJob(w http.ResponseWriter, r *http.Request, rc *reqCtx) error {
	j, res, err := u.visibleJob(r, rc)
	if err != nil {
		return err
	}
	if !rc.can(auth.ActRetryJob, res) {
		return u.deny(r, rc, "not allowed on this job")
	}
	if err := u.srv.RetryJob(r.Context(), rc.P, j.ID); err != nil {
		return err
	}
	u.redirect(w, r, "/ui/jobs/"+url.PathEscape(j.ID))
	return nil
}
