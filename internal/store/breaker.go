package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
)

// The worker circuit breaker (DESIGN.md §4.2). A worker's failure streak is
// the run of its leases that ended failed (retryable) or expired since its
// last completed task, kept per job in worker_failure_streak:
//   - a non-retryable failure is the input's fault (ffmpeg rejected the
//     upload, say), not the worker's, so it neither counts nor resets;
//   - a release or a cancellation is nobody's fault and does neither;
//   - a completed task (CompleteTranscribe, AcceptPrepare) ends the streak,
//     and so does making the worker active (resume, approval).
//
// When an active worker's streak reaches Config.WorkerFailureLimit failures
// across at least two jobs, the transaction that ended the lease also pauses
// the worker, records why, and appends the worker.pause audit entry with
// actor "system". Two jobs, so that one poisoned input that crashes every
// worker it meets cannot take a healthy worker out of service. A paused
// worker keeps its current leases (they may still finish) and claims nothing
// new until an administrator resumes it.

// AutoPause describes a worker the circuit breaker just paused.
type AutoPause struct {
	WorkerID string
	// Failures and Jobs are the streak that tripped the breaker.
	Failures, Jobs int
	// Reason is the stored pause reason, e.g. "3 consecutive failures
	// across 2 jobs".
	Reason string
}

// trackFailureStreak counts a lease end in its worker's failure streak and
// pauses the worker if that trips the breaker, returning what it paused (nil
// if nothing).
func (s *Store) trackFailureStreak(ctx context.Context, tx *sql.Tx, l leaseRow, outcome api.LeaseOutcome,
	retryable bool, now time.Time) (*AutoPause, error) {
	if outcome != api.OutcomeExpired && (outcome != api.OutcomeFailed || !retryable) {
		return nil, nil
	}
	// A worker that no longer exists is skipped, as for its counters.
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_failure_streak (worker_id, job_id, failures)
		SELECT id, ?, 1 FROM workers WHERE id = ?
		ON CONFLICT (worker_id, job_id) DO UPDATE SET failures = failures + 1`, l.JobID, l.WorkerID); err != nil {
		return nil, fmt.Errorf("failure streak: %w", err)
	}
	limit := s.cfg.WorkerFailureLimit
	if limit <= 0 {
		return nil, nil
	}
	var state api.WorkerState
	var failures, jobs int
	err := tx.QueryRowContext(ctx, `SELECT w.state, COALESCE(SUM(f.failures), 0), COUNT(f.job_id)
		FROM workers w LEFT JOIN worker_failure_streak f ON f.worker_id = w.id
		WHERE w.id = ? GROUP BY w.id`, l.WorkerID).Scan(&state, &failures, &jobs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failure streak: %w", err)
	}
	if state != api.WorkerActive || failures < limit || jobs < 2 {
		return nil, nil
	}
	p := &AutoPause{WorkerID: l.WorkerID, Failures: failures, Jobs: jobs,
		Reason: fmt.Sprintf("%d consecutive failures across %d jobs", failures, jobs)}
	if _, err := tx.ExecContext(ctx, `UPDATE workers SET state = 'paused', decided_by = ?, decided_at = ?,
		pause_reason = ? WHERE id = ?`, SystemActor, toMS(now), p.Reason, l.WorkerID); err != nil {
		return nil, fmt.Errorf("auto-pause worker: %w", err)
	}
	// The same detail shape as an administrator's worker.pause, plus why.
	detail, err := json.Marshal(map[string]any{"from": api.WorkerActive, "cancelled_leases": 0,
		"reason": p.Reason, "failures": failures, "jobs": jobs})
	if err != nil {
		return nil, fmt.Errorf("auto-pause worker: %w", err)
	}
	if _, err := (&Tx{s: s, tx: tx}).AppendAudit(ctx, AuditEntry{TS: now, Actor: SystemActor,
		Action: "worker.pause", Target: WorkerPrincipal(l.WorkerID), Detail: string(detail)}); err != nil {
		return nil, err
	}
	return p, nil
}

// clearFailureStreak ends a worker's failure streak.
func clearFailureStreak(ctx context.Context, tx *sql.Tx, workerID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM worker_failure_streak WHERE worker_id = ?`, workerID); err != nil {
		return fmt.Errorf("clear failure streak: %w", err)
	}
	return nil
}
