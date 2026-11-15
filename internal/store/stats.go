package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
)

// Stats is a consistent snapshot for the metrics gauges (DESIGN.md §10.2).
type Stats struct {
	Jobs    []JobCount
	Tasks   []TaskCount
	Queues  []QueueStats
	Workers map[api.WorkerState]int
	// OriginalBytes and CanonicalBytes sum the unpurged media sizes.
	OriginalBytes  int64
	CanonicalBytes int64
}

// JobCount is the number of jobs in one queue and state.
type JobCount struct {
	Queue string
	State api.JobState
	Count int
}

// TaskCount is the number of tasks in one queue, kind and state. State may
// be "cancelled" in addition to the api.TaskState values.
type TaskCount struct {
	Queue string
	Kind  api.TaskKind
	State api.TaskState
	Count int
}

// QueueStats are the per-queue scheduling gauges.
type QueueStats struct {
	ID     int64
	Name   string
	Weight int
	// VTime is the queue's virtual time; ClassVTimes the per-class ones.
	VTime       float64
	ClassVTimes map[api.Weight]float64
	// BacklogCost sums the cost of pending tasks.
	BacklogCost  float64
	PendingTasks int
	// OldestPending is the age of the oldest job with a pending task, 0 if
	// nothing is pending.
	OldestPending time.Duration
	// EligibleWorkers counts active workers seen within the window given to
	// Stats that hold process on this queue (or on "*").
	EligibleWorkers int
}

// Stats takes a snapshot of job, task, queue, worker and storage gauges.
// seenWithin is the liveness window for QueueStats.EligibleWorkers.
func (s *Store) Stats(ctx context.Context, seenWithin time.Duration) (*Stats, error) {
	now := s.now()
	tx, err := s.r.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("stats: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	st := &Stats{Workers: map[api.WorkerState]int{}}
	queues := map[int64]*QueueStats{}
	err = eachRow(ctx, tx, `SELECT id, name, weight FROM queues ORDER BY name`, nil, func(r *sql.Rows) error {
		var q QueueStats
		if err := r.Scan(&q.ID, &q.Name, &q.Weight); err != nil {
			return err
		}
		q.ClassVTimes = map[api.Weight]float64{}
		st.Queues = append(st.Queues, q)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range st.Queues {
		queues[st.Queues[i].ID] = &st.Queues[i]
	}

	steps := []struct {
		q    string
		args []any
		scan func(*sql.Rows) error
	}{
		{`SELECT q.name, j.state, COUNT(*) FROM jobs j JOIN queues q ON q.id = j.queue_id
			GROUP BY q.name, j.state ORDER BY q.name, j.state`, nil, func(r *sql.Rows) error {
			var c JobCount
			if err := r.Scan(&c.Queue, &c.State, &c.Count); err != nil {
				return err
			}
			st.Jobs = append(st.Jobs, c)
			return nil
		}},
		{`SELECT q.name, t.kind, t.state, COUNT(*) FROM tasks t JOIN jobs j ON j.id = t.job_id
			JOIN queues q ON q.id = j.queue_id GROUP BY q.name, t.kind, t.state ORDER BY 1, 2, 3`, nil,
			func(r *sql.Rows) error {
				var c TaskCount
				if err := r.Scan(&c.Queue, &c.Kind, &c.State, &c.Count); err != nil {
					return err
				}
				st.Tasks = append(st.Tasks, c)
				return nil
			}},
		{`SELECT j.queue_id, COUNT(*), SUM(t.cost), MIN(j.created_at) FROM tasks t JOIN jobs j ON j.id = t.job_id
			WHERE t.state = 'pending' GROUP BY j.queue_id`, nil, func(r *sql.Rows) error {
			var id, oldest int64
			var n int
			var cost float64
			if err := r.Scan(&id, &n, &cost, &oldest); err != nil {
				return err
			}
			if q := queues[id]; q != nil {
				q.PendingTasks, q.BacklogCost = n, cost
				q.OldestPending = max(0, now.Sub(fromMS(oldest)))
			}
			return nil
		}},
		{`SELECT queue_id, class, vtime FROM sched_vtime`, nil, func(r *sql.Rows) error {
			var id int64
			var class api.Weight
			var v float64
			if err := r.Scan(&id, &class, &v); err != nil {
				return err
			}
			if q := queues[id]; q != nil {
				if class == "" {
					q.VTime = v
				} else {
					q.ClassVTimes[class] = v
				}
			}
			return nil
		}},
		{`SELECT state, COUNT(*) FROM workers GROUP BY state`, nil, func(r *sql.Rows) error {
			var state api.WorkerState
			var n int
			if err := r.Scan(&state, &n); err != nil {
				return err
			}
			st.Workers[state] = n
			return nil
		}},
		{`SELECT q.id, COUNT(DISTINCT w.id) FROM queues q JOIN grants g
			ON g.role = ? AND (g.scope = '*' OR g.scope = 'queue:' || q.id)
			JOIN workers w ON g.principal = 'worker:' || w.id
			WHERE w.state = 'active' AND w.last_seen_at >= ? GROUP BY q.id`,
			[]any{string(api.RoleProcess), toMS(now.Add(-seenWithin))}, func(r *sql.Rows) error {
				var id int64
				var n int
				if err := r.Scan(&id, &n); err != nil {
					return err
				}
				if q := queues[id]; q != nil {
					q.EligibleWorkers = n
				}
				return nil
			}},
		{`SELECT COALESCE(SUM(CASE WHEN original_purged_at IS NULL THEN original_bytes END), 0),
			COALESCE(SUM(CASE WHEN canonical_purged_at IS NULL THEN canonical_bytes END), 0) FROM jobs`, nil,
			func(r *sql.Rows) error { return r.Scan(&st.OriginalBytes, &st.CanonicalBytes) }},
	}
	for _, step := range steps {
		if err := eachRow(ctx, tx, step.q, step.args, step.scan); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// eachRow runs q and calls fn for every row.
func eachRow(ctx context.Context, q querier, query string, args []any, fn func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return fmt.Errorf("stats: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	return nil
}
