package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
)

// queueNameRE is the queue name grammar from DESIGN.md §4.1.
var queueNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidQueueName reports whether name is a legal queue name.
func ValidQueueName(name string) bool { return queueNameRE.MatchString(name) }

// Queue is a stored queue.
type Queue struct {
	ID          int64
	Name        string
	Description string
	Weight      int
	// MaxUploadBytes is the per-queue upload cap; 0 means the server default.
	MaxUploadBytes int64
	CreatedAt      time.Time
}

// API returns the wire representation of q.
func (q *Queue) API() api.Queue {
	return api.Queue{Name: q.Name, Description: q.Description, Weight: q.Weight, MaxUploadBytes: q.MaxUploadBytes}
}

// NewQueue holds the fields of a queue to create. Zero Weight means
// DefaultQueueWeight.
type NewQueue struct {
	Name           string
	Description    string
	Weight         int
	MaxUploadBytes int64
}

func validWeight(w int) error {
	if w < 1 || w > 10000 {
		return fmt.Errorf("%w: queue weight %d not in 1..10000", ErrInvalid, w)
	}
	return nil
}

// CreateQueue creates a queue. It returns ErrConflict if the name is taken.
func (s *Store) CreateQueue(ctx context.Context, nq NewQueue) (*Queue, error) {
	return inTx1(ctx, s, func(tx *Tx) (*Queue, error) { return tx.CreateQueue(ctx, nq) })
}

// CreateQueue is Store.CreateQueue inside the transaction.
func (t *Tx) CreateQueue(ctx context.Context, nq NewQueue) (*Queue, error) {
	if !ValidQueueName(nq.Name) {
		return nil, fmt.Errorf("%w: queue name %q", ErrInvalid, nq.Name)
	}
	if nq.Weight == 0 {
		nq.Weight = DefaultQueueWeight
	}
	if err := validWeight(nq.Weight); err != nil {
		return nil, err
	}
	if nq.MaxUploadBytes < 0 {
		return nil, fmt.Errorf("%w: negative max_upload_bytes", ErrInvalid)
	}
	return t.s.createQueue(ctx, t.tx, nq)
}

func (s *Store) createQueue(ctx context.Context, tx *sql.Tx, nq NewQueue) (*Queue, error) {
	now := s.now()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO queues (name, description, weight, max_upload_bytes, created_at) VALUES (?,?,?,?,?)`,
		nq.Name, nq.Description, nq.Weight, sql.NullInt64{Int64: nq.MaxUploadBytes, Valid: nq.MaxUploadBytes > 0}, toMS(now))
	if isUnique(err) {
		return nil, fmt.Errorf("create queue %q: %w", nq.Name, ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("create queue %q: %w", nq.Name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create queue %q: %w", nq.Name, err)
	}
	return &Queue{ID: id, Name: nq.Name, Description: nq.Description, Weight: nq.Weight,
		MaxUploadBytes: nq.MaxUploadBytes, CreatedAt: fromMS(toMS(now))}, nil
}

const queueCols = `id, name, description, weight, COALESCE(max_upload_bytes, 0), created_at`

func scanQueue(row interface{ Scan(...any) error }) (*Queue, error) {
	var q Queue
	var created int64
	if err := row.Scan(&q.ID, &q.Name, &q.Description, &q.Weight, &q.MaxUploadBytes, &created); err != nil {
		return nil, err
	}
	q.CreatedAt = fromMS(created)
	return &q, nil
}

// QueueByName returns the queue with the given name.
func (s *Store) QueueByName(ctx context.Context, name string) (*Queue, error) {
	q, err := scanQueue(s.r.QueryRowContext(ctx, `SELECT `+queueCols+` FROM queues WHERE name = ?`, name))
	if err != nil {
		return nil, notFound(err, "queue "+name)
	}
	return q, nil
}

// QueueByID returns the queue with the given id.
func (s *Store) QueueByID(ctx context.Context, id int64) (*Queue, error) {
	return queueByID(ctx, s.r, id)
}

func queueByID(ctx context.Context, db querier, id int64) (*Queue, error) {
	q, err := scanQueue(db.QueryRowContext(ctx, `SELECT `+queueCols+` FROM queues WHERE id = ?`, id))
	if err != nil {
		return nil, notFound(err, fmt.Sprintf("queue %d", id))
	}
	return q, nil
}

// ListQueues returns all queues ordered by name.
func (s *Store) ListQueues(ctx context.Context) ([]Queue, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+queueCols+` FROM queues ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list queues: %w", err)
	}
	defer rows.Close()
	var out []Queue
	for rows.Next() {
		q, err := scanQueue(rows)
		if err != nil {
			return nil, fmt.Errorf("list queues: %w", err)
		}
		out = append(out, *q)
	}
	return out, rows.Err()
}

// PatchQueue applies the non-nil fields of p to queue id and returns the
// result. A MaxUploadBytes of 0 resets the cap to the server default.
// Authorization (only admin may change weight) is the caller's job.
func (s *Store) PatchQueue(ctx context.Context, id int64, p api.QueuePatch) (*Queue, error) {
	return inTx1(ctx, s, func(tx *Tx) (*Queue, error) { return tx.PatchQueue(ctx, id, p) })
}

// PatchQueue is Store.PatchQueue inside the transaction.
func (t *Tx) PatchQueue(ctx context.Context, id int64, p api.QueuePatch) (*Queue, error) {
	if p.Weight != nil {
		if err := validWeight(*p.Weight); err != nil {
			return nil, err
		}
	}
	if p.MaxUploadBytes != nil && *p.MaxUploadBytes < 0 {
		return nil, fmt.Errorf("%w: negative max_upload_bytes", ErrInvalid)
	}
	tx := t.tx
	if p.Description != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE queues SET description = ? WHERE id = ?`, *p.Description, id); err != nil {
			return nil, fmt.Errorf("patch queue: %w", err)
		}
	}
	if p.Weight != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE queues SET weight = ? WHERE id = ?`, *p.Weight, id); err != nil {
			return nil, fmt.Errorf("patch queue: %w", err)
		}
	}
	if p.MaxUploadBytes != nil {
		v := sql.NullInt64{Int64: *p.MaxUploadBytes, Valid: *p.MaxUploadBytes > 0}
		if _, err := tx.ExecContext(ctx, `UPDATE queues SET max_upload_bytes = ? WHERE id = ?`, v, id); err != nil {
			return nil, fmt.Errorf("patch queue: %w", err)
		}
	}
	return queueByID(ctx, tx, id)
}

// RenameQueue renames queue id. ErrConflict if the new name is taken.
func (s *Store) RenameQueue(ctx context.Context, id int64, name string) error {
	return s.InTx(ctx, func(tx *Tx) error { return tx.RenameQueue(ctx, id, name) })
}

// RenameQueue is Store.RenameQueue inside the transaction.
func (t *Tx) RenameQueue(ctx context.Context, id int64, name string) error {
	if !ValidQueueName(name) {
		return fmt.Errorf("%w: queue name %q", ErrInvalid, name)
	}
	res, err := t.tx.ExecContext(ctx, `UPDATE queues SET name = ? WHERE id = ?`, name, id)
	if isUnique(err) {
		return fmt.Errorf("rename queue to %q: %w", name, ErrConflict)
	}
	return mustAffect(res, err, "rename queue")
}
