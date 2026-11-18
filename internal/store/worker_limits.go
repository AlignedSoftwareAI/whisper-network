package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
)

// ErrPendingFull is returned by RegisterWorkerCapped when the number of
// workers awaiting approval has reached the cap.
var ErrPendingFull = errors.New("store: too many workers awaiting approval")

// ErrPendingSourceFull is returned by RegisterWorkerCapped when the source
// address already has its cap of workers awaiting approval.
var ErrPendingSourceFull = errors.New("store: too many workers awaiting approval from this source")

// PendingLimits caps registrations awaiting approval. Zero caps are off.
type PendingLimits struct {
	// Total caps all pending workers.
	Total int
	// PerSource caps the pending workers registered from Source.
	PerSource int
	// Source is the registering address's key (auth.IPKey); it is stored
	// on the worker. With Source empty PerSource does not apply.
	Source string
}

// RegisterWorkerCapped is RegisterWorker that records lim.Source and fails
// with ErrPendingSourceFull or ErrPendingFull when a cap of lim is already
// reached. The counts and the insert share one write transaction, so
// concurrent registrations cannot overshoot either cap.
func (s *Store) RegisterWorkerCapped(ctx context.Context, tokenHash string, facts api.WorkerFacts, lim PendingLimits) (*Worker, error) {
	return inTx1(ctx, s, func(tx *Tx) (*Worker, error) {
		return tx.RegisterWorkerCapped(ctx, tokenHash, facts, lim)
	})
}

// RegisterWorkerCapped is Store.RegisterWorkerCapped inside the
// transaction.
func (t *Tx) RegisterWorkerCapped(ctx context.Context, tokenHash string, facts api.WorkerFacts, lim PendingLimits) (*Worker, error) {
	if lim.PerSource > 0 && lim.Source != "" {
		var n int
		if err := t.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workers WHERE state = 'pending' AND registered_from = ?`,
			lim.Source).Scan(&n); err != nil {
			return nil, fmt.Errorf("count pending workers: %w", err)
		}
		if n >= lim.PerSource {
			return nil, ErrPendingSourceFull
		}
	}
	if lim.Total > 0 {
		var n int
		if err := t.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workers WHERE state = 'pending'`).Scan(&n); err != nil {
			return nil, fmt.Errorf("count pending workers: %w", err)
		}
		if n >= lim.Total {
			return nil, ErrPendingFull
		}
	}
	return t.registerWorker(ctx, tokenHash, facts, lim.Source)
}

// PrunedWorker is a worker deleted by DeleteRejectedWorkers or
// DeleteRejectedWorker, with the counters deleted with it.
type PrunedWorker struct {
	ID    string
	Stats api.WorkerStats
}

// DeleteRejectedWorkers deletes workers rejected before cutoff, with every
// grant they hold or that is scoped to them and their counters, and returns
// how many. Until then a rejected worker's token still earns it a
// "rejected" answer, telling it to exit; afterwards the token is merely
// unknown.
func (s *Store) DeleteRejectedWorkers(ctx context.Context, cutoff time.Time) (int64, error) {
	ws, err := inTx1(ctx, s, func(tx *Tx) ([]PrunedWorker, error) { return tx.DeleteRejectedWorkers(ctx, cutoff) })
	return int64(len(ws)), err
}

// DeleteRejectedWorkers is Store.DeleteRejectedWorkers inside the
// transaction. It returns the deleted workers with their final counters
// (deleted with them by ON DELETE CASCADE), so the caller can record them.
func (t *Tx) DeleteRejectedWorkers(ctx context.Context, cutoff time.Time) ([]PrunedWorker, error) {
	const where = `state = 'rejected' AND COALESCE(decided_at, registered_at) < ?`
	ws, err := t.deleteRejected(ctx, rejectedDeletion{
		stats:   `w.` + where,
		grants:  deleteGrantsOf + where + `) OR scope IN (SELECT 'worker:' || id FROM workers WHERE ` + where + `)`,
		workers: `DELETE FROM workers WHERE ` + where + ` RETURNING id`,
	}, toMS(cutoff))
	if err != nil {
		return nil, fmt.Errorf("delete rejected workers: %w", err)
	}
	return ws, nil
}

// DeleteRejectedWorker deletes one rejected worker on demand, as
// DeleteRejectedWorkers does past retention: with every grant it holds or
// that is scoped to it, and its counters. It returns the worker with its
// final counters, or ErrNotFound if there is no rejected worker with that
// id (the caller checks the state first to tell the two apart).
func (t *Tx) DeleteRejectedWorker(ctx context.Context, id string) (*PrunedWorker, error) {
	const where = `state = 'rejected' AND id = ?`
	ws, err := t.deleteRejected(ctx, rejectedDeletion{
		stats:   `w.state = 'rejected' AND w.id = ?`,
		grants:  deleteGrantsOf + where + `) OR scope IN (SELECT 'worker:' || id FROM workers WHERE ` + where + `)`,
		workers: `DELETE FROM workers WHERE ` + where + ` RETURNING id`,
	}, id)
	if err != nil {
		return nil, fmt.Errorf("delete rejected worker %s: %w", id, err)
	}
	if len(ws) == 0 {
		return nil, fmt.Errorf("rejected worker %s: %w", id, ErrNotFound)
	}
	return &ws[0], nil
}

// deleteGrantsOf starts the statement deleting the grants held by, or
// scoped to, the workers selected by a condition.
const deleteGrantsOf = `DELETE FROM grants WHERE principal IN (SELECT 'worker:' || id FROM workers WHERE `

// rejectedDeletion is the SQL deleting a set of rejected workers, each
// selecting them by the same condition with one "?": their counters (for
// statsWhere), their grants, then the workers themselves. The statements
// are built from constants by the callers.
type rejectedDeletion struct {
	stats, grants, workers string
}

// deleteRejected runs q with arg bound to each "?", and returns the
// deleted workers with the counters deleted with them.
func (t *Tx) deleteRejected(ctx context.Context, q rejectedDeletion, arg any) ([]PrunedWorker, error) {
	stats, err := t.statsWhere(ctx, q.stats, arg)
	if err != nil {
		return nil, err
	}
	if _, err := t.tx.ExecContext(ctx, q.grants, arg, arg); err != nil {
		return nil, err
	}
	rows, err := t.tx.QueryContext(ctx, q.workers, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PrunedWorker
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, PrunedWorker{ID: id, Stats: stats[id]})
	}
	return out, rows.Err()
}
