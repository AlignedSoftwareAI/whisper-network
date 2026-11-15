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

// DeleteRejectedWorkers deletes workers rejected before cutoff, with every
// grant they hold or that is scoped to them, and returns how many. Until
// then a rejected worker's token still earns it a "rejected" answer, telling
// it to exit; afterwards the token is merely unknown.
func (s *Store) DeleteRejectedWorkers(ctx context.Context, cutoff time.Time) (int64, error) {
	ids, err := inTx1(ctx, s, func(tx *Tx) ([]string, error) { return tx.DeleteRejectedWorkers(ctx, cutoff) })
	return int64(len(ids)), err
}

// DeleteRejectedWorkers is Store.DeleteRejectedWorkers inside the
// transaction. It returns the deleted workers' ids.
func (t *Tx) DeleteRejectedWorkers(ctx context.Context, cutoff time.Time) ([]string, error) {
	const where = `state = 'rejected' AND COALESCE(decided_at, registered_at) < ?`
	ms := toMS(cutoff)
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM grants
		WHERE principal IN (SELECT 'worker:' || id FROM workers WHERE `+where+`)
		   OR scope IN (SELECT 'worker:' || id FROM workers WHERE `+where+`)`, ms, ms); err != nil {
		return nil, fmt.Errorf("delete rejected workers: %w", err)
	}
	rows, err := t.tx.QueryContext(ctx, `DELETE FROM workers WHERE `+where+` RETURNING id`, ms)
	if err != nil {
		return nil, fmt.Errorf("delete rejected workers: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("delete rejected workers: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
