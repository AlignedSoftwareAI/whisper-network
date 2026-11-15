package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/deweysasser/whisper-network/internal/api"
)

// Bootstrap ensures the "public" queue and the "admin" user exist. If it
// creates the admin user it stores adminPasswordHash, sets
// must_change_password, grants admin on "*", and reports created = true so
// the caller knows to reveal the password. An existing admin is left as is.
// Whatever it creates is audited with actor SystemActor in the same
// transaction ("queue.create", "user.create").
func (s *Store) Bootstrap(ctx context.Context, adminPasswordHash string) (created bool, err error) {
	err = s.InTx(ctx, func(t *Tx) error {
		tx := t.tx
		q, err := s.createQueue(ctx, tx, NewQueue{Name: PublicQueue, Weight: DefaultQueueWeight})
		switch {
		case err == nil:
			if err := t.auditSystem(ctx, "queue.create", "queue:"+q.Name, `{"bootstrap":true}`); err != nil {
				return err
			}
		case !errors.Is(err, ErrConflict):
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE username = ?`, AdminUsername).Scan(&n); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		if n > 0 {
			return nil
		}
		u, err := s.createUser(ctx, tx, AdminUsername, adminPasswordHash, true)
		if err != nil {
			return err
		}
		created = true
		if err := addGrant(ctx, tx, u.Principal(), api.RoleAdmin, ScopeAll, SystemActor, s.now()); err != nil {
			return err
		}
		return t.auditSystem(ctx, "user.create", "user:"+u.Username,
			`{"bootstrap":true,"grants":[{"role":"admin","scope":"*"}]}`)
	})
	return created, err
}

func (t *Tx) auditSystem(ctx context.Context, action, target, detail string) error {
	_, err := t.AppendAudit(ctx, AuditEntry{Actor: SystemActor, Action: action, Target: target, Detail: detail})
	return err
}
