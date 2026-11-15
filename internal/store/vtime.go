package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/sched"
)

// loadSched reads every virtual clock. There are a handful of rows per queue,
// so loading them all is cheaper than computing which ones a claim needs.
func loadSched(ctx context.Context, q querier) (*sched.State, error) {
	st := &sched.State{V: map[sched.Key]float64{}, Last: map[int64]float64{}}
	rows, err := q.QueryContext(ctx, `SELECT queue_id, class, vtime FROM sched_vtime`)
	if err != nil {
		return nil, fmt.Errorf("load vtimes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k sched.Key
		var v float64
		if err := rows.Scan(&k.Queue, &k.Class, &v); err != nil {
			return nil, fmt.Errorf("load vtimes: %w", err)
		}
		st.V[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load vtimes: %w", err)
	}
	lrows, err := q.QueryContext(ctx, `SELECT scope, vtime FROM sched_last`)
	if err != nil {
		return nil, fmt.Errorf("load last vtimes: %w", err)
	}
	defer lrows.Close()
	for lrows.Next() {
		var scope int64
		var v float64
		if err := lrows.Scan(&scope, &v); err != nil {
			return nil, fmt.Errorf("load last vtimes: %w", err)
		}
		st.Last[scope] = v
	}
	return st, lrows.Err()
}

// saveSched upserts the given clocks of st and every Last entry.
func saveSched(ctx context.Context, tx *sql.Tx, st *sched.State, keys ...sched.Key) error {
	for _, k := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sched_vtime (queue_id, class, vtime) VALUES (?,?,?)
			ON CONFLICT (queue_id, class) DO UPDATE SET vtime = excluded.vtime`,
			k.Queue, string(k.Class), st.Get(k)); err != nil {
			return fmt.Errorf("save vtime: %w", err)
		}
	}
	for scope, v := range st.Last {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sched_last (scope, vtime) VALUES (?,?)
			ON CONFLICT (scope) DO UPDATE SET vtime = excluded.vtime`, scope, v); err != nil {
			return fmt.Errorf("save last vtime: %w", err)
		}
	}
	return nil
}

// backlogged returns the (queue, class) pairs that currently have pending tasks.
func backlogged(ctx context.Context, q querier) ([]sched.Key, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT j.queue_id, j.weight FROM tasks t
		JOIN jobs j ON j.id = t.job_id WHERE t.state = 'pending'`)
	if err != nil {
		return nil, fmt.Errorf("backlogged: %w", err)
	}
	defer rows.Close()
	var out []sched.Key
	for rows.Next() {
		var k sched.Key
		if err := rows.Scan(&k.Queue, &k.Class); err != nil {
			return nil, fmt.Errorf("backlogged: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// reactivate applies the scheduler's reactivation rule for work about to
// become pending in (queueID, class). It must run before the tasks are
// inserted or set to pending, so the backlog it sees is the "before" state.
func reactivate(ctx context.Context, tx *sql.Tx, queueID int64, class api.Weight) error {
	busy, err := backlogged(ctx, tx)
	if err != nil {
		return err
	}
	st, err := loadSched(ctx, tx)
	if err != nil {
		return err
	}
	k := sched.Key{Queue: queueID, Class: class}
	st.Reactivate(k, busy)
	return saveSched(ctx, tx, st, sched.QueueKey(queueID), k)
}
