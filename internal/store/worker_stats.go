package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
)

// Per-worker counters (table worker_stats). They change only in the
// transaction that ends a lease, so they always agree with task outcomes:
//   - a completed task (CompleteTranscribe, AcceptPrepare) counts by kind,
//     with its compute time, its audio (transcribe), and its job if it is
//     the worker's first completed task in that job; a transcribe task's
//     audio also counts in the fleet's throughput (throughput.go);
//   - a lease ended failed (Fail) or expired (ExpireLeases) counts as a
//     failure or an expiry;
//   - a release or a cancellation (worker rejected, job failed) counts as
//     nothing: neither is the worker's fault.

// statsDelta is one change to a worker's counters.
type statsDelta struct {
	prepare, transcribe, jobs int64
	audioMS                   int64
	prepareMS, transcribeMS   int64
	failures, expired         int64
	completedAt               sql.NullInt64 // set for a completion
}

// bumpWorkerStats adds d to the worker's counters, creating its row. A
// worker that no longer exists is skipped rather than failing the lease
// transaction.
func bumpWorkerStats(ctx context.Context, tx *sql.Tx, workerID string, d statsDelta) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO worker_stats (worker_id, prepare_done, transcribe_done, jobs,
		audio_ms, prepare_compute_ms, transcribe_compute_ms, failures, expired, first_task_at, last_task_at)
		SELECT id, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? FROM workers WHERE id = ?
		ON CONFLICT (worker_id) DO UPDATE SET
		  prepare_done = prepare_done + excluded.prepare_done,
		  transcribe_done = transcribe_done + excluded.transcribe_done,
		  jobs = jobs + excluded.jobs,
		  audio_ms = audio_ms + excluded.audio_ms,
		  prepare_compute_ms = prepare_compute_ms + excluded.prepare_compute_ms,
		  transcribe_compute_ms = transcribe_compute_ms + excluded.transcribe_compute_ms,
		  failures = failures + excluded.failures,
		  expired = expired + excluded.expired,
		  first_task_at = COALESCE(first_task_at, excluded.first_task_at),
		  last_task_at = COALESCE(excluded.last_task_at, last_task_at)`,
		d.prepare, d.transcribe, d.jobs, d.audioMS, d.prepareMS, d.transcribeMS, d.failures, d.expired,
		d.completedAt, d.completedAt, workerID)
	if err != nil {
		return fmt.Errorf("worker stats: %w", err)
	}
	return nil
}

// countCompletion counts the task of l, just marked done by its worker with
// computeMS, in the worker's counters, and ends the worker's failure streak.
//
// The job counts if no other task of the job is done by this worker. Done
// tasks never change state or worker again and are deleted only with their
// job, so this counts each (worker, job) pair exactly once, with no extra
// table to keep.
func countCompletion(ctx context.Context, tx *sql.Tx, l leaseRow, computeMS int64, now time.Time) error {
	d := statsDelta{completedAt: sql.NullInt64{Int64: toMS(now), Valid: true}}
	var span int64
	var seenJob bool
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(t.end_sample - t.start_sample, 0),
		EXISTS (SELECT 1 FROM tasks o WHERE o.job_id = t.job_id AND o.seq != t.seq
		        AND o.state = 'done' AND o.worker_id = ?)
		FROM tasks t WHERE t.job_id = ? AND t.seq = ?`, l.WorkerID, l.JobID, l.Seq).Scan(&span, &seenJob); err != nil {
		return fmt.Errorf("worker stats: %w", err)
	}
	if !seenJob {
		d.jobs = 1
	}
	switch l.Kind {
	case api.KindPrepare:
		d.prepare, d.prepareMS = 1, computeMS
	case api.KindTranscribe:
		d.transcribe, d.transcribeMS = 1, computeMS
		d.audioMS = span * 1000 / canonicalSampleRate
		if err := countThroughput(ctx, tx, l.QueueID, d.audioMS, now); err != nil {
			return err
		}
	}
	if err := clearFailureStreak(ctx, tx, l.WorkerID); err != nil {
		return err
	}
	return bumpWorkerStats(ctx, tx, l.WorkerID, d)
}

// countLeaseEnd counts a lease ended other than by completion: failed and
// expired count, released and cancelled do not.
func countLeaseEnd(ctx context.Context, tx *sql.Tx, workerID string, outcome api.LeaseOutcome) error {
	var d statsDelta
	switch outcome {
	case api.OutcomeFailed:
		d.failures = 1
	case api.OutcomeExpired:
		d.expired = 1
	default:
		return nil
	}
	return bumpWorkerStats(ctx, tx, workerID, d)
}

// workerStatsCols are the worker_stats columns scanned by scanStats, from a
// LEFT JOIN aliased ws (so NULL for a worker with no row).
const workerStatsCols = `COALESCE(ws.prepare_done, 0), COALESCE(ws.transcribe_done, 0), COALESCE(ws.jobs, 0),
	COALESCE(ws.audio_ms, 0), COALESCE(ws.prepare_compute_ms, 0), COALESCE(ws.transcribe_compute_ms, 0),
	COALESCE(ws.failures, 0), COALESCE(ws.expired, 0), ws.first_task_at, ws.last_task_at`

// statsScan holds the raw columns of workerStatsCols.
type statsScan struct {
	prepare, transcribe, jobs, audioMS, prepareMS, transcribeMS, failures, expired int64
	first, last                                                                    sql.NullInt64
}

func (s *statsScan) dest() []any {
	return []any{&s.prepare, &s.transcribe, &s.jobs, &s.audioMS, &s.prepareMS, &s.transcribeMS,
		&s.failures, &s.expired, &s.first, &s.last}
}

func (s *statsScan) stats() api.WorkerStats {
	return api.WorkerStats{
		PrepareTasks:       s.prepare,
		TranscribeTasks:    s.transcribe,
		Jobs:               s.jobs,
		AudioS:             float64(s.audioMS) / 1000,
		ComputeS:           float64(s.prepareMS+s.transcribeMS) / 1000,
		TranscribeComputeS: float64(s.transcribeMS) / 1000,
		Failures:           s.failures,
		ExpiredLeases:      s.expired,
		FirstTaskAt:        optTime(s.first),
		LastTaskAt:         optTime(s.last),
	}
}

// statsWhere returns the counters of the workers (aliased w) matching cond,
// by worker id.
func (t *Tx) statsWhere(ctx context.Context, cond string, args ...any) (map[string]api.WorkerStats, error) {
	rows, err := t.tx.QueryContext(ctx, `SELECT w.id, `+workerStatsCols+` FROM workers w
		LEFT JOIN worker_stats ws ON ws.worker_id = w.id WHERE `+cond, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]api.WorkerStats{}
	for rows.Next() {
		var id string
		var st statsScan
		if err := rows.Scan(append([]any{&id}, st.dest()...)...); err != nil {
			return nil, err
		}
		out[id] = st.stats()
	}
	return out, rows.Err()
}
