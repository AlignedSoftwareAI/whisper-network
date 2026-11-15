package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
)

// WorkerRef names a worker for display: its id and the self-reported
// hostname and description (either may be empty).
type WorkerRef struct {
	ID          string
	Hostname    string
	Description string
}

// LiveLease is a live lease (not ended, not past its expiry) joined to its
// task, job and worker, for display. It carries the job's queue and owner
// so callers can apply job visibility.
type LiveLease struct {
	ID     string
	JobID  string
	Seq    int
	Kind   api.TaskKind
	Worker WorkerRef
	// Attempt is the task's attempt count including this lease.
	Attempt   int
	QueueName string
	ClientID  string // the job's owner
	CreatedAt time.Time
	ExpiresAt time.Time
	// HeartbeatAt is the time of the latest heartbeat, nil if none yet.
	HeartbeatAt *time.Time
}

// liveLeaseSelect selects LiveLease rows; the caller appends a condition
// over l (leases) and an ORDER BY. The first argument is now (ms).
const liveLeaseSelect = `SELECT l.id, l.job_id, l.seq, t.kind, t.attempts, l.worker_id,
	COALESCE(w.hostname, ''), COALESCE(w.description, ''), q.name, j.client_id,
	l.created_at, l.expires_at, l.heartbeat_at
	FROM leases l
	JOIN tasks t ON t.job_id = l.job_id AND t.seq = l.seq
	JOIN jobs j ON j.id = l.job_id
	JOIN queues q ON q.id = j.queue_id
	LEFT JOIN workers w ON w.id = l.worker_id
	WHERE l.ended_at IS NULL AND l.expires_at > ? AND `

// LiveLeasesForJobs returns the live leases on the given jobs' tasks,
// ordered by job and task. It reads the partial index leases_one_live.
func (s *Store) LiveLeasesForJobs(ctx context.Context, jobIDs []string) ([]LiveLease, error) {
	return s.liveLeasesWhere(ctx, liveByJob, jobIDs)
}

// The two ways to select live leases: column matched and result order.
var (
	liveByJob    = liveLeaseKey{col: "l.job_id", order: "l.job_id, l.seq"}
	liveByWorker = liveLeaseKey{col: "l.worker_id", order: "l.worker_id, l.created_at, l.id"}
)

type liveLeaseKey struct{ col, order string }

// LiveLeasesForWorkers returns the live leases held by the given workers,
// ordered by worker and lease age. It reads the partial index
// leases_worker_live.
func (s *Store) LiveLeasesForWorkers(ctx context.Context, workerIDs []string) ([]LiveLease, error) {
	return s.liveLeasesWhere(ctx, liveByWorker, workerIDs)
}

// query is the live-lease query for n ids.
func (k liveLeaseKey) query(n int) string {
	return liveLeaseSelect + k.col + ` IN (` + placeholders(n) + `) ORDER BY ` + k.order
}

func (s *Store) liveLeasesWhere(ctx context.Context, k liveLeaseKey, ids []string) ([]LiveLease, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, toMS(s.now()))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.r.QueryContext(ctx, k.query(len(ids)), args...)
	if err != nil {
		return nil, fmt.Errorf("live leases: %w", err)
	}
	defer rows.Close()
	var out []LiveLease
	for rows.Next() {
		var l LiveLease
		var created, expires int64
		var hb sql.NullInt64
		if err := rows.Scan(&l.ID, &l.JobID, &l.Seq, &l.Kind, &l.Attempt, &l.Worker.ID, &l.Worker.Hostname,
			&l.Worker.Description, &l.QueueName, &l.ClientID, &created, &expires, &hb); err != nil {
			return nil, fmt.Errorf("live leases: %w", err)
		}
		l.CreatedAt, l.ExpiresAt, l.HeartbeatAt = fromMS(created), fromMS(expires), optTime(hb)
		out = append(out, l)
	}
	return out, rows.Err()
}

// JobWorker is a worker that completed at least one task of a job.
type JobWorker struct {
	JobID  string
	Worker WorkerRef
}

// CompletedTaskWorkers returns, for the given jobs, each distinct worker
// that completed one of their tasks (tasks.worker_id of done tasks),
// ordered by job and then by the worker's first completed task.
func (s *Store) CompletedTaskWorkers(ctx context.Context, jobIDs []string) ([]JobWorker, error) {
	if len(jobIDs) == 0 {
		return nil, nil
	}
	args := make([]any, len(jobIDs))
	for i, id := range jobIDs {
		args[i] = id
	}
	rows, err := s.r.QueryContext(ctx, `SELECT t.job_id, t.worker_id, COALESCE(w.hostname, ''),
		COALESCE(w.description, '')
		FROM tasks t LEFT JOIN workers w ON w.id = t.worker_id
		WHERE t.job_id IN (`+placeholders(len(jobIDs))+`) AND t.state = 'done' AND t.worker_id IS NOT NULL
		GROUP BY t.job_id, t.worker_id ORDER BY t.job_id, MIN(t.seq)`, args...)
	if err != nil {
		return nil, fmt.Errorf("completed task workers: %w", err)
	}
	defer rows.Close()
	var out []JobWorker
	for rows.Next() {
		var jw JobWorker
		if err := rows.Scan(&jw.JobID, &jw.Worker.ID, &jw.Worker.Hostname, &jw.Worker.Description); err != nil {
			return nil, fmt.Errorf("completed task workers: %w", err)
		}
		out = append(out, jw)
	}
	return out, rows.Err()
}
