package ui

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/store"
)

// jobView is the job detail page.
type jobView struct {
	CSRF        string
	Job         api.Job
	Owner       string
	Tasks       []taskRow
	Running     []runningRow
	CanDelete   bool
	CanDownload bool // a done job the viewer may read (auth.ActDownloadTranscript)
	// CanPartial offers the partial transcript (#66): a job that is not
	// done, has finished transcribe tasks, and that the viewer may read.
	CanPartial bool
	// CanRetry offers Retry (#56): a failed job with its media, and a viewer
	// who may retry it (auth.ActRetryJob).
	CanRetry bool
}

// taskRow is one finished task. Running tasks are runningRows; pending
// tasks appear as a count on the job.
type taskRow struct {
	Seq           int
	Kind          api.TaskKind
	StartS, EndS  float64
	WorkerID      string
	EngineVersion string
	ComputeS      float64
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
	j, res, err := u.visibleJob(r, rc)
	if err != nil {
		return err
	}
	outs, err := u.srv.Store().JobOutputs(r.Context(), j.ID)
	if err != nil {
		return err
	}
	canRead := rc.can(auth.ActDownloadTranscript, res)
	v := jobView{CSRF: rc.CSRF, Job: u.srv.JobDoc(r.Context(), rc.P.Principal, j), Owner: j.ClientID,
		CanDelete:   rc.can(auth.ActDeleteJob, res),
		CanDownload: j.State == api.JobDone && canRead,
		CanPartial:  j.State != api.JobDone && canRead && len(outs.Transcribe) > 0,
		CanRetry:    j.Retryable() && rc.can(auth.ActRetryJob, res)}
	if p := outs.Prepare; p != nil {
		v.Tasks = append(v.Tasks, taskRow{Seq: p.Seq, Kind: api.KindPrepare, WorkerID: p.WorkerID,
			EngineVersion: p.EngineVersion, ComputeS: float64(p.ComputeMS) / 1000})
	}
	for _, t := range outs.Transcribe {
		v.Tasks = append(v.Tasks, taskRow{Seq: t.Seq, Kind: api.KindTranscribe,
			StartS: samplesToSeconds(t.StartSample), EndS: samplesToSeconds(t.EndSample),
			WorkerID: t.WorkerID, EngineVersion: t.EngineVersion, ComputeS: float64(t.ComputeMS) / 1000})
	}
	if v.Running, err = u.runningRows(r.Context(), rc, j); err != nil {
		return err
	}
	u.render(w, r, rc, http.StatusOK, "job", "Job", v)
	return nil
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
