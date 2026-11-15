package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Ping runs an empty write transaction, proving the database is writable
// (the server's /readyz check).
func (s *Store) Ping(ctx context.Context) error {
	return s.write(ctx, func(*sql.Tx) error { return nil })
}

// WorkerTokenRejected reports whether hash belongs to a rejected worker.
// WorkerByTokenHash treats such tokens as unknown; the server uses this to
// answer them with 410 so the worker knows to stop rather than retry.
func (s *Store) WorkerTokenRejected(ctx context.Context, hash string) (bool, error) {
	var n int
	err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM workers WHERE token_hash = ? AND state = 'rejected'`,
		hash).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("worker token rejected: %w", err)
	}
	return n > 0, nil
}

// JobsAwaitingAssembly returns jobs that are not terminal although every
// transcribe task is done: the server crashed (or failed to write the
// transcript) between the last result and MarkJobDone. The reaper assembles
// them again.
func (s *Store) JobsAwaitingAssembly(ctx context.Context) ([]string, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT j.id FROM jobs j WHERE j.state IN ('queued','running')
		AND EXISTS (SELECT 1 FROM tasks t WHERE t.job_id = j.id AND t.kind = 'transcribe')
		AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.job_id = j.id AND t.kind = 'transcribe' AND t.state != 'done')
		ORDER BY j.created_at`)
	if err != nil {
		return nil, fmt.Errorf("jobs awaiting assembly: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("jobs awaiting assembly: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
