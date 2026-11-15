package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AuditEntry is one audit log record.
type AuditEntry struct {
	ID     int64
	TS     time.Time
	Actor  string
	Action string
	Target string
	Detail string
}

// AppendAudit appends an entry in its own transaction. A zero TS means now.
// Returns the entry's id. To record a change atomically with the change
// itself, use Tx.AppendAudit inside InTx.
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) (int64, error) {
	return inTx1(ctx, s, func(tx *Tx) (int64, error) { return tx.AppendAudit(ctx, e) })
}

// AppendAudit appends an entry as part of the transaction. A zero TS means
// now. Returns the entry's id.
func (t *Tx) AppendAudit(ctx context.Context, e AuditEntry) (int64, error) {
	if e.Actor == "" || e.Action == "" {
		return 0, fmt.Errorf("%w: audit entry needs actor and action", ErrInvalid)
	}
	if e.TS.IsZero() {
		e.TS = t.s.now()
	}
	res, err := t.tx.ExecContext(ctx, `INSERT INTO audit (ts, actor, action, target, detail) VALUES (?,?,?,?,?)`,
		toMS(e.TS), e.Actor, e.Action, e.Target, e.Detail)
	if err != nil {
		return 0, fmt.Errorf("append audit: %w", err)
	}
	return res.LastInsertId()
}

// AuditFilter selects audit entries. Zero fields do not filter.
type AuditFilter struct {
	// Before returns only entries with an id below it (paging backwards:
	// pass the last returned ID).
	Before int64
	// Limit caps the page; values outside 1..1000 mean 100.
	Limit int
	// ActionPrefix matches actions starting with it, e.g. "grant." or
	// "auth.login".
	ActionPrefix string
	// Actor and Target match exactly.
	Actor, Target string
	// Since and Until bound the entry time: Since <= ts < Until.
	Since, Until time.Time
}

// ListAudit returns up to limit entries newest first, starting below the
// entry id before (0 = from the newest). Pass the last returned ID as before
// to page backwards.
func (s *Store) ListAudit(ctx context.Context, before int64, limit int) ([]AuditEntry, error) {
	return s.QueryAudit(ctx, AuditFilter{Before: before, Limit: limit})
}

// QueryAudit returns the entries matching f, newest first.
func (s *Store) QueryAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	var where []string
	var args []any
	add := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	if f.Before > 0 {
		add(`id < ?`, f.Before)
	}
	if f.ActionPrefix != "" {
		// substr avoids LIKE wildcards in the prefix ("_" is common in actions).
		add(`substr(action, 1, length(?)) = ?`, f.ActionPrefix, f.ActionPrefix)
	}
	if f.Actor != "" {
		add(`actor = ?`, f.Actor)
	}
	if f.Target != "" {
		add(`target = ?`, f.Target)
	}
	if !f.Since.IsZero() {
		add(`ts >= ?`, toMS(f.Since))
	}
	if !f.Until.IsZero() {
		add(`ts < ?`, toMS(f.Until))
	}
	q := `SELECT id, ts, actor, action, target, detail FROM audit`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	rows, err := s.r.QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, append(args, f.Limit)...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, fmt.Errorf("list audit: %w", err)
		}
		e.TS = fromMS(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
