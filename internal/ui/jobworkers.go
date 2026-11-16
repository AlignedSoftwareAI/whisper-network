package ui

import (
	"context"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/store"
)

// Which worker runs a job is shown only to principals holding
// auth.ActViewJobWorkers (admin, queue_admin on the job's queue, or
// worker_admin over the worker); anyone else who can see the job sees only
// that its tasks are running (DESIGN.md §5.3).

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
	Workers []ref
	Running int // live leases whose worker is not shown
}

// jobRow is one row of a queue's job table.
type jobRow struct {
	api.Job
	Ref     ref // the job, named by its label (#58)
	Workers jobWorkers
	// Waiting is how long the job's oldest pending task has waited, set
	// only while it has pending tasks and none running (#54).
	Waiting time.Duration
}

// queueJobRows builds the job table rows for jobs on queue: a fixed number
// of indexed queries for the whole page, whatever its length.
func (u *UI) queueJobRows(ctx context.Context, rc *reqCtx, queue string, jobs []store.Job) ([]jobRow, error) {
	docs := u.srv.JobDocuments(ctx, rc.P.Principal, jobs)
	rows := make([]jobRow, len(jobs))
	byID := make(map[string]*jobRow, len(jobs))
	var active, finished []string
	for i := range jobs {
		rows[i].Job = docs[i]
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
			r.Workers.Workers = append(r.Workers.Workers, workerRef(l.Worker))
		}
	}
	done, err := st.CompletedTaskWorkers(ctx, finished)
	if err != nil {
		return nil, err
	}
	for _, jw := range done {
		if rc.seesWorker(queue, jw.Worker) {
			r := byID[jw.JobID]
			r.Workers.Workers = append(r.Workers.Workers, workerRef(jw.Worker))
		}
	}
	ids := labelIDs{jobs: append(active, finished...)}
	lb, err := u.labels(ctx, rc, ids)
	if err != nil {
		return nil, err
	}
	since, err := st.JobsPendingSince(ctx, active)
	if err != nil {
		return nil, err
	}
	now := u.srv.Now()
	for i := range rows {
		rows[i].Ref = lb.job(rows[i].ID)
		if t, ok := since[rows[i].ID]; ok && rows[i].Tasks.Leased == 0 {
			rows[i].Waiting = max(now.Sub(t), 0)
		}
	}
	return rows, nil
}

// runningRow is one running task on the job page. Worker is nil when the
// principal may not see it.
type runningRow struct {
	Seq         int
	Kind        api.TaskKind
	Worker      *ref
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
			wr := workerRef(l.Worker)
			out[i].Worker = &wr
		}
	}
	return out, nil
}

// workerLease is a live lease on the workers page.
type workerLease struct {
	Job   ref // named by its label (#58)
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
	var ids labelIDs
	for _, l := range live {
		ids.job(l.JobID)
	}
	lb, err := u.labels(ctx, rc, ids)
	if err != nil {
		return nil, nil, err
	}
	shown, hidden := map[string][]workerLease{}, map[string]int{}
	for _, l := range live {
		if !rc.can(auth.ActViewJob, auth.Resource{Queue: l.QueueName, OwnerID: l.ClientID}) {
			hidden[l.Worker.ID]++
			continue
		}
		shown[l.Worker.ID] = append(shown[l.Worker.ID], workerLease{Job: lb.job(l.JobID), JobID: l.JobID, Seq: l.Seq,
			Kind: l.Kind, Age: now.Sub(l.CreatedAt)})
	}
	return shown, hidden, nil
}
