package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/deweysasser/whisper-network/internal/api"
)

// NotRetryableError says why a job cannot be retried: it is not failed, or
// the media its retry needs has been purged. It wraps ErrConflict.
type NotRetryableError struct{ Reason string }

func (e *NotRetryableError) Error() string { return "not retryable: " + e.Reason }

// Unwrap makes errors.Is(err, ErrConflict) true.
func (e *NotRetryableError) Unwrap() error { return ErrConflict }

// IsNotRetryable reports whether err is a NotRetryableError, and its reason.
func IsNotRetryable(err error) (string, bool) {
	var nr *NotRetryableError
	if errors.As(err, &nr) {
		return nr.Reason, true
	}
	return "", false
}

// Retried is the outcome of a job retry.
type Retried struct {
	// PreviousError is the error the failed job carried.
	PreviousError string
	// State is the job's new state: preparing or queued.
	State api.JobState
	// Tasks is how many tasks went back to pending.
	Tasks int
	// Retries is the job's retry count after this one.
	Retries int
}

// retryBlocker returns why a job cannot be retried, or "" if it can: it
// must be failed, and the media its retry reads must still be on disk (the
// canonical audio once prepare succeeded, else the original upload).
func (j *Job) retryBlocker() string {
	switch {
	case j.State != api.JobFailed:
		return "job is " + string(j.State) + "; only failed jobs can be retried"
	case j.Canonical != nil && j.CanonicalPurgedAt != nil:
		return "the job's audio has been purged; submit the media again"
	case j.Canonical == nil && (j.OriginalPurgedAt != nil || j.Original.Bytes == 0):
		return "the job's original upload has been purged; submit the media again"
	}
	return ""
}

// Retryable reports whether the job can be retried now (DESIGN.md §4.2).
func (j *Job) Retryable() bool { return j.retryBlocker() == "" }

// RetryJob retries a failed job (DESIGN.md §4.2): its done tasks and their
// results are kept; its failed and cancelled tasks go back to pending with a
// fresh attempt budget and no retry-elsewhere exclusions (earlier leases are
// kept for history but no longer count, see leases.retry); its error and
// completion time are cleared; it returns to preparing (prepare never
// succeeded) or queued, and its retry count goes up. A job that is not
// failed, or whose needed media has been purged, is a NotRetryableError and
// nothing changes.
func (t *Tx) RetryJob(ctx context.Context, jobID string) (*Retried, error) {
	tx := t.tx
	j, err := getJob(ctx, tx, jobID)
	if err != nil {
		return nil, err
	}
	if why := j.retryBlocker(); why != "" {
		return nil, &NotRetryableError{Reason: why}
	}
	out := &Retried{PreviousError: j.Error, State: api.JobQueued, Retries: j.Retries + 1}
	if j.Canonical == nil {
		out.State = api.JobPreparing
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET state = 'pending', attempts = 0, worker_id = NULL
		WHERE job_id = ? AND state IN ('failed','cancelled')`, jobID)
	if err != nil {
		return nil, fmt.Errorf("retry job %s: %w", jobID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	out.Tasks = int(n)
	if err := reactivate(ctx, tx, j.QueueID, j.Weight); err != nil {
		return nil, err
	}
	res, err = tx.ExecContext(ctx, `UPDATE jobs SET state = ?, error = NULL, completed_at = NULL, compute_ms = NULL,
		retries = retries + 1, updated_at = ? WHERE id = ? AND state = 'failed'`,
		string(out.State), toMS(t.s.now()), jobID)
	if err := mustAffect(res, err, "retry job "+jobID); err != nil {
		return nil, err
	}
	return out, nil
}

// RetryJob is Tx.RetryJob in its own transaction.
func (s *Store) RetryJob(ctx context.Context, jobID string) (*Retried, error) {
	return inTx1(ctx, s, func(tx *Tx) (*Retried, error) { return tx.RetryJob(ctx, jobID) })
}

// FailedMedia is a failed job's retained media, for the failed-job
// retention policy (DESIGN.md §8.5).
type FailedMedia struct {
	JobID     string
	Original  bool  // the original upload is still present
	Canonical bool  // the canonical audio is still present
	Bytes     int64 // the size of what is present
}

// FailedJobMedia returns the failed jobs that still hold media, oldest
// failure first.
func (s *Store) FailedJobMedia(ctx context.Context) ([]FailedMedia, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, original_purged_at IS NULL AND original_bytes > 0,
		canonical_purged_at IS NULL AND canonical_bytes IS NOT NULL,
		CASE WHEN original_purged_at IS NULL THEN original_bytes ELSE 0 END
		  + CASE WHEN canonical_purged_at IS NULL THEN COALESCE(canonical_bytes, 0) ELSE 0 END
		FROM jobs WHERE state = 'failed'
		AND ((original_purged_at IS NULL AND original_bytes > 0)
		  OR (canonical_purged_at IS NULL AND canonical_bytes IS NOT NULL))
		ORDER BY completed_at, id`)
	if err != nil {
		return nil, fmt.Errorf("failed job media: %w", err)
	}
	defer rows.Close()
	var out []FailedMedia
	for rows.Next() {
		var m FailedMedia
		if err := rows.Scan(&m.JobID, &m.Original, &m.Canonical, &m.Bytes); err != nil {
			return nil, fmt.Errorf("failed job media: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed job media: %w", err)
	}
	return out, nil
}

// FailedStats is the failed-job retention gauges: bytes of media held for
// failed jobs, and how many failed jobs can still be retried.
type FailedStats struct {
	Bytes     int64
	Retryable int
}

// FailedJobStats returns the failed-job retention gauges.
func (s *Store) FailedJobStats(ctx context.Context) (FailedStats, error) {
	var st FailedStats
	err := s.r.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN original_purged_at IS NULL THEN original_bytes ELSE 0 END
		  + CASE WHEN canonical_purged_at IS NULL THEN COALESCE(canonical_bytes, 0) ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN canonical_bytes IS NOT NULL THEN canonical_purged_at IS NULL
		  ELSE original_purged_at IS NULL AND original_bytes > 0 END), 0)
		FROM jobs WHERE state = 'failed'`).Scan(&st.Bytes, &st.Retryable)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, fmt.Errorf("failed job stats: %w", err)
	}
	return st, nil
}
