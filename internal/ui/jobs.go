package ui

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
)

// jobView is the job detail page.
type jobView struct {
	CSRF        string
	Job         api.Job
	Owner       string
	Tasks       []taskRow
	CanDelete   bool
	CanDownload bool // a done job the viewer may read (auth.ActDownloadTranscript)
}

// taskRow is one finished task. The store exposes finished task outputs
// only; pending and leased tasks appear as counts on the job.
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
	v := jobView{CSRF: rc.CSRF, Job: server.JobDocument(rc.P.Principal, j, u.srv.Now()), Owner: j.ClientID,
		CanDelete:   rc.can(auth.ActDeleteJob, res),
		CanDownload: j.State == api.JobDone && rc.can(auth.ActDownloadTranscript, res)}
	if p := outs.Prepare; p != nil {
		v.Tasks = append(v.Tasks, taskRow{Seq: p.Seq, Kind: api.KindPrepare, WorkerID: p.WorkerID,
			EngineVersion: p.EngineVersion, ComputeS: float64(p.ComputeMS) / 1000})
	}
	for _, t := range outs.Transcribe {
		v.Tasks = append(v.Tasks, taskRow{Seq: t.Seq, Kind: api.KindTranscribe,
			StartS: samplesToSeconds(t.StartSample), EndS: samplesToSeconds(t.EndSample),
			WorkerID: t.WorkerID, EngineVersion: t.EngineVersion, ComputeS: float64(t.ComputeMS) / 1000})
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
