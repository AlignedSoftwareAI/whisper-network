package ui

import (
	"context"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
)

// Which worker runs a job is shown only to principals holding
// auth.ActViewJobWorkers (admin, queue_admin on the job's queue, or
// worker_admin over the worker); anyone else who can see the job sees only
// that its tasks are running (DESIGN.md §5.3).

// workerLink is a worker named on a job page, linking to the worker's page
// (/ui/workers/<id>).
type workerLink struct {
	ID   string
	Name string
}

// refName names a worker the way the workers pages do (workerName).
func refName(w store.WorkerRef) string {
	return workerName(&store.Worker{ID: w.ID, Facts: api.WorkerFacts{Hostname: w.Hostname, Description: w.Description}})
}

// seesWorker reports whether rc may see that worker w processes jobs on
// queue.
func (rc *reqCtx) seesWorker(queue string, w store.WorkerRef) bool {
	return rc.can(auth.ActViewJobWorkers, auth.Resource{Queue: queue, WorkerID: w.ID})
}

// jobWorkers is the Workers cell of a job row. A running job lists the
// workers holding its live leases; a done or failed job lists the workers
// that completed its tasks. Workers the principal may not see are only
// counted, as Running for live leases (completed ones are omitted).
type jobWorkers struct {
	Workers []workerLink
	Running int // live leases whose worker is not shown
}

// jobRow is one row of a queue's job table.
type jobRow struct {
	api.Job
	Workers jobWorkers
}

// queueJobRows builds the job table rows for jobs on queue: two indexed
// queries for the whole page, whatever its length.
func (u *UI) queueJobRows(ctx context.Context, rc *reqCtx, queue string, jobs []store.Job) ([]jobRow, error) {
	now := u.srv.Now()
	rows := make([]jobRow, len(jobs))
	byID := make(map[string]*jobRow, len(jobs))
	var active, finished []string
	for i := range jobs {
		rows[i].Job = server.JobDocument(rc.P.Principal, &jobs[i], now)
		byID[jobs[i].ID] = &rows[i]
		if jobs[i].Terminal() {
			finished = append(finished, jobs[i].ID)
		} else {
			active = append(active, jobs[i].ID)
		}
	}
	st := u.srv.Store()
	live, err := st.LiveLeasesForJobs(ctx, active)
	if err != nil {
		return nil, err
	}
	seen := map[[2]string]bool{}
	for _, l := range live {
		r := byID[l.JobID]
		switch {
		case !rc.seesWorker(queue, l.Worker):
			r.Workers.Running++
		case !seen[[2]string{l.JobID, l.Worker.ID}]:
			seen[[2]string{l.JobID, l.Worker.ID}] = true
			r.Workers.Workers = append(r.Workers.Workers, workerLink{ID: l.Worker.ID, Name: refName(l.Worker)})
		}
	}
	done, err := st.CompletedTaskWorkers(ctx, finished)
	if err != nil {
		return nil, err
	}
	for _, jw := range done {
		if rc.seesWorker(queue, jw.Worker) {
			r := byID[jw.JobID]
			r.Workers.Workers = append(r.Workers.Workers, workerLink{ID: jw.Worker.ID, Name: refName(jw.Worker)})
		}
	}
	return rows, nil
}

// runningRow is one running task on the job page. Worker is nil when the
// principal may not see it.
type runningRow struct {
	Seq         int
	Kind        api.TaskKind
	Worker      *workerLink
	Age         time.Duration
	HeartbeatAt *time.Time
	Attempt     int
}

// runningRows lists the job's live leases.
func (u *UI) runningRows(ctx context.Context, rc *reqCtx, j *store.Job) ([]runningRow, error) {
	if j.Terminal() {
		return nil, nil
	}
	live, err := u.srv.Store().LiveLeasesForJobs(ctx, []string{j.ID})
	if err != nil {
		return nil, err
	}
	now := u.srv.Now()
	out := make([]runningRow, len(live))
	for i, l := range live {
		out[i] = runningRow{Seq: l.Seq, Kind: l.Kind, Age: now.Sub(l.CreatedAt), HeartbeatAt: l.HeartbeatAt,
			Attempt: l.Attempt}
		if rc.seesWorker(j.QueueName, l.Worker) {
			out[i].Worker = &workerLink{ID: l.Worker.ID, Name: refName(l.Worker)}
		}
	}
	return out, nil
}

// workerLease is a live lease on the workers page.
type workerLease struct {
	JobID string
	Seq   int
	Kind  api.TaskKind
	Age   time.Duration
}

// workerLeases groups live leases by worker. Leases on jobs the principal
// may not view are only counted (in the second map), so the worker page
// never reveals job ids from queues the principal cannot see.
func (u *UI) workerLeases(ctx context.Context, rc *reqCtx, workerIDs []string) (map[string][]workerLease, map[string]int, error) {
	live, err := u.srv.Store().LiveLeasesForWorkers(ctx, workerIDs)
	if err != nil {
		return nil, nil, err
	}
	now := u.srv.Now()
	shown, hidden := map[string][]workerLease{}, map[string]int{}
	for _, l := range live {
		if !rc.can(auth.ActViewJob, auth.Resource{Queue: l.QueueName, OwnerID: l.ClientID}) {
			hidden[l.Worker.ID]++
			continue
		}
		shown[l.Worker.ID] = append(shown[l.Worker.ID], workerLease{JobID: l.JobID, Seq: l.Seq, Kind: l.Kind,
			Age: now.Sub(l.CreatedAt)})
	}
	return shown, hidden, nil
}
