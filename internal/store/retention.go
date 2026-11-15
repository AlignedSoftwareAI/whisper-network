package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MediaPurge names a finished job whose media should be removed.
type MediaPurge struct {
	JobID     string
	Original  bool // the original upload is still present
	Canonical bool // the canonical audio is still present
}

// MediaToPurge returns terminal (done or failed) jobs that finished at or
// before cutoff (now − audio_retention) and still have original or canonical
// media. Callers delete the blobs, then call MarkOriginalPurged /
// MarkCanonicalPurged.
func (s *Store) MediaToPurge(ctx context.Context, cutoff time.Time) ([]MediaPurge, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, original_purged_at IS NULL AND original_bytes > 0,
		canonical_purged_at IS NULL AND canonical_bytes IS NOT NULL
		FROM jobs WHERE state IN ('done','failed') AND completed_at <= ?
		AND ((original_purged_at IS NULL AND original_bytes > 0)
		  OR (canonical_purged_at IS NULL AND canonical_bytes IS NOT NULL))
		ORDER BY completed_at`, toMS(cutoff))
	if err != nil {
		return nil, fmt.Errorf("media to purge: %w", err)
	}
	defer rows.Close()
	var out []MediaPurge
	for rows.Next() {
		var p MediaPurge
		if err := rows.Scan(&p.JobID, &p.Original, &p.Canonical); err != nil {
			return nil, fmt.Errorf("media to purge: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// JobsToDelete returns terminal jobs that finished at or before cutoff
// (now − job_retention). Callers remove blobs, then call DeleteJob.
func (s *Store) JobsToDelete(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id FROM jobs WHERE state IN ('done','failed') AND completed_at <= ?
		ORDER BY completed_at`, toMS(cutoff))
	if err != nil {
		return nil, fmt.Errorf("jobs to delete: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("jobs to delete: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteEndedLeases deletes leases that ended before cutoff and returns how
// many. Leases of a task that is still pending or leased are kept whatever
// their age: they record which workers failed it, which decides who may
// take it next and how many attempts it has left (DESIGN.md §8.2).
func (s *Store) DeleteEndedLeases(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE ended_at IS NOT NULL AND ended_at < ?
			AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.job_id = leases.job_id AND t.seq = leases.seq
				AND t.state IN ('pending','leased'))`, toMS(cutoff))
		if err != nil {
			return fmt.Errorf("delete ended leases: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
