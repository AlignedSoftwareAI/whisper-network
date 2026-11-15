package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/blobs"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/store"
)

// visibleJob loads a job the principal may see, then checks act. A job the
// caller cannot see is reported as 404, never 403, so IDs cannot be probed
// (DESIGN.md §5.3); a visible job the caller may not act on is 403.
func (s *Server) visibleJob(r *http.Request, p *Principal, act auth.Action) (*store.Job, error) {
	j, err := s.st.Job(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		return nil, err
	}
	res := auth.Resource{Queue: j.QueueName, OwnerID: j.ClientID}
	if !auth.Can(p.Principal, auth.ActViewJob, res) {
		return nil, s.hide(r, p, "job not visible")
	}
	if act != auth.ActViewJob && !auth.Can(p.Principal, act, res) {
		return nil, s.deny(r, p, "not allowed on this job")
	}
	return j, nil
}

// JobDocument builds the job status document as p may see it. The
// client-supplied content (prompt and metadata) is left out unless p may
// read the job (auth.ActViewJobContent: its owner, read_all on its queue, or
// admin); view_all and queue_admin alone see status only. The caller must
// already have checked that p may see the job at all.
func JobDocument(p auth.Principal, j *store.Job, now time.Time) api.Job {
	doc := j.API(now)
	if !auth.Can(p, auth.ActViewJobContent, auth.Resource{Queue: j.QueueName, OwnerID: j.ClientID}) {
		doc.Prompt, doc.Metadata = "", nil
	}
	return doc
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request, p *Principal) error {
	j, err := s.visibleJob(r, p, auth.ActViewJob)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, JobDocument(p.Principal, j, s.now()))
	return nil
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request, p *Principal) error {
	q, err := s.st.QueueByName(r.Context(), r.PathValue("q"))
	if err != nil {
		return err
	}
	f := store.JobFilter{QueueID: q.ID, Cursor: r.URL.Query().Get("cursor")}
	switch auth.VisibilityOn(p.Principal, q.Name) {
	case auth.SeesNone:
		return s.deny(r, p, "no access to queue "+q.Name)
	case auth.SeesOwn:
		f.ClientID = p.ID
	}
	qv := r.URL.Query()
	if v := qv.Get("state"); v != "" {
		for _, st := range strings.Split(v, ",") {
			js := api.JobState(strings.TrimSpace(st))
			switch js {
			case api.JobPreparing, api.JobQueued, api.JobRunning, api.JobDone, api.JobFailed:
				f.States = append(f.States, js)
			default:
				return badRequest("unknown state " + strconv.Quote(st))
			}
		}
	}
	if v := qv.Get("since"); v != "" {
		t, err := parseTime(v)
		if err != nil {
			return badRequest("since must be RFC 3339 or Unix seconds")
		}
		f.Since = t
	}
	if v := qv.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return badRequest("limit must be a positive integer")
		}
		f.Limit = n
	}
	jobs, next, err := s.st.ListJobs(r.Context(), f)
	if err != nil {
		return err
	}
	out := api.JobList{Jobs: make([]api.Job, len(jobs)), NextCursor: next}
	now := s.now()
	for i := range jobs {
		out.Jobs[i] = JobDocument(p.Principal, &jobs[i], now)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func parseTime(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, nil
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(int64(secs * 1000)).UTC(), nil
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request, p *Principal) error {
	j, err := s.visibleJob(r, p, auth.ActDeleteJob)
	if err != nil {
		return err
	}
	if err := s.deleteJob(r.Context(), p.Actor(), j.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// deleteJob removes a job's row (cancelling its live leases, whose holders
// then get 410), audited as job.delete by actor, and then its blobs. The
// row goes first: a crash in between leaves an orphan directory that the
// sweep removes, never a row whose media has vanished.
func (s *Server) deleteJob(ctx context.Context, actor Actor, jobID string) error {
	// Hold the job lock so a delete cannot interleave with prepare
	// acceptance or transcript assembly, which write into the job's blob
	// directory: either they finish first and their files are removed here,
	// or the delete wins and they find the job gone.
	unlock := s.locks.Lock(jobID)
	defer unlock()
	cancelled, err := s.ops.DeleteJob(ctx, actor, jobID)
	if err != nil {
		return err
	}
	if err := s.blobs.RemoveJob(jobID); err != nil {
		s.log.Warn().Err(err).Str(obs.FieldJobID, jobID).Msg("removing job blobs failed; the sweep will retry")
	}
	obs.Info(&s.log, obs.EventJobDeleted).Str(obs.FieldJobID, jobID).Str(obs.FieldActor, actor.Name).
		Int("cancelled_leases", len(cancelled)).Msg("job deleted")
	return nil
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request, p *Principal) error {
	return s.serveOutput(w, r, p, blobs.Transcript, "text/plain; charset=utf-8")
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request, p *Principal) error {
	return s.serveOutput(w, r, p, blobs.Result, "application/json")
}

// serveOutput serves an assembled output of a done job and records the
// download.
func (s *Server) serveOutput(w http.ResponseWriter, r *http.Request, p *Principal, name, ctype string) error {
	j, err := s.visibleJob(r, p, auth.ActDownloadTranscript)
	if err != nil {
		return err
	}
	if j.State != api.JobDone {
		p := newProblem(http.StatusConflict, api.ProblemNotDone, "job is "+string(j.State))
		p.state = string(j.State)
		return p
	}
	f, err := s.blobs.Open(j.ID, name)
	if errors.Is(err, fs.ErrNotExist) {
		return newProblem(http.StatusNotFound, api.ProblemNotFound, "output not found")
	}
	if err != nil {
		return err
	}
	defer f.Close()
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, no-cache")
	modTime := time.Time{}
	if j.CompletedAt != nil {
		modTime = *j.CompletedAt
	}
	if fi, err := f.Stat(); err == nil {
		s.extendWrite(w, fi.Size())
	}
	dw := &downloadWriter{ResponseWriter: w}
	http.ServeContent(dw, r, "", modTime, f)
	// Only a delivered body is a download: not HEAD, not 304 or 412, not
	// a transfer the client abandoned.
	if r.Method != http.MethodGet || dw.err != nil ||
		(dw.status != http.StatusOK && dw.status != http.StatusPartialContent) {
		return nil
	}
	if err := s.st.RecordDownload(context.WithoutCancel(r.Context()), j.ID); err != nil {
		s.log.Warn().Err(err).Str(obs.FieldJobID, j.ID).Msg("recording a download failed")
	}
	return nil
}

// downloadWriter records a response's status and whether writing it failed.
type downloadWriter struct {
	http.ResponseWriter
	status int
	err    error
}

func (d *downloadWriter) WriteHeader(code int) {
	if d.status == 0 {
		d.status = code
	}
	d.ResponseWriter.WriteHeader(code)
}

func (d *downloadWriter) Write(b []byte) (int, error) {
	if d.status == 0 {
		d.status = http.StatusOK
	}
	n, err := d.ResponseWriter.Write(b)
	if err != nil && d.err == nil {
		d.err = err
	}
	return n, err
}

func (d *downloadWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }
