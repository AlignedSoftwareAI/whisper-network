package store

import (
	"context"
	"database/sql"
)

// Tx is one write transaction. Its methods are the store's write operations,
// so several of them (typically a change plus its audit entry) commit
// together or not at all. A Tx is valid only inside the InTx callback that
// received it, and is not safe for concurrent use.
//
// The writer has a single connection, so code inside InTx must not call
// Store methods that write (they would wait for this transaction forever).
// Reads through the Store use the separate read pool and do not see the
// transaction's uncommitted changes.
type Tx struct {
	s  *Store
	tx *sql.Tx
}

// InTx runs fn in one BEGIN IMMEDIATE write transaction. If fn returns an
// error, or the commit fails, every change made through tx is rolled back
// and the error is returned.
func (s *Store) InTx(ctx context.Context, fn func(tx *Tx) error) error {
	return s.write(ctx, func(tx *sql.Tx) error { return fn(&Tx{s: s, tx: tx}) })
}

// inTx1 is InTx for a write that returns one value, so the Store wrappers
// around Tx methods stay one line.
func inTx1[T any](ctx context.Context, s *Store, fn func(tx *Tx) (T, error)) (T, error) {
	var out T
	err := s.InTx(ctx, func(tx *Tx) error {
		var err error
		out, err = fn(tx)
		return err
	})
	return out, err
}
