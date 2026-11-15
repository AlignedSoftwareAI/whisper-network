package store

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

// TestInTxAtomic: a change and its audit entry commit together, and an
// error anywhere in the callback rolls back every write made through tx.
func TestInTxAtomic(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	before, err := f.s.ListAudit(f.ctx, 0, 100)
	require.NoError(t, err)

	boom := errors.New("boom")
	err = f.s.InTx(f.ctx, func(tx *Tx) error {
		if _, err := tx.CreateQueue(f.ctx, NewQueue{Name: "rolled-back"}); err != nil {
			return err
		}
		if _, err := tx.AppendAudit(f.ctx, AuditEntry{Actor: "a", Action: "queue.create"}); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	_, err = f.s.QueueByName(f.ctx, "rolled-back")
	require.ErrorIs(t, err, ErrNotFound)

	// An invalid audit entry fails the whole change.
	err = f.s.InTx(f.ctx, func(tx *Tx) error {
		if _, err := tx.CreateQueue(f.ctx, NewQueue{Name: "also-rolled-back"}); err != nil {
			return err
		}
		_, err := tx.AppendAudit(f.ctx, AuditEntry{})
		return err
	})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = f.s.QueueByName(f.ctx, "also-rolled-back")
	require.ErrorIs(t, err, ErrNotFound)

	after, err := f.s.ListAudit(f.ctx, 0, 100)
	require.NoError(t, err)
	assert.Len(t, after, len(before), "no audit entry survives a rollback")

	require.NoError(t, f.s.InTx(f.ctx, func(tx *Tx) error {
		if _, err := tx.CreateQueue(f.ctx, NewQueue{Name: "kept"}); err != nil {
			return err
		}
		_, err := tx.AppendAudit(f.ctx, AuditEntry{Actor: "a", Action: "queue.create", Target: "queue:kept"})
		return err
	}))
	_, err = f.s.QueueByName(f.ctx, "kept")
	require.NoError(t, err)
	got, err := f.s.QueryAudit(f.ctx, AuditFilter{Target: "queue:kept"})
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestQueryAuditFilters(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	t0 := f.clk.Now()
	add := func(actor, action, target string) {
		t.Helper()
		_, err := f.s.AppendAudit(f.ctx, AuditEntry{Actor: actor, Action: action, Target: target})
		require.NoError(t, err)
		f.clk.Advance(time.Minute)
	}
	add("user:alice", "grant.add", "client:c1")
	add("user:alice", "grant.remove", "client:c1")
	add("user:bob", "grant_x.odd", "client:c2") // "_" must not act as a LIKE wildcard
	add("system", "auth.login_failed", "user:carol")

	actions := func(f AuditFilter, fx *fixture) []string {
		t.Helper()
		es, err := fx.s.QueryAudit(fx.ctx, f)
		require.NoError(t, err)
		var out []string
		for _, e := range es {
			out = append(out, e.Action)
		}
		return out
	}
	assert.Equal(t, []string{"grant.remove", "grant.add"}, actions(AuditFilter{ActionPrefix: "grant."}, f))
	assert.Equal(t, []string{"grant.remove", "grant.add"}, actions(AuditFilter{Actor: "user:alice"}, f))
	assert.Equal(t, []string{"grant_x.odd"}, actions(AuditFilter{Target: "client:c2"}, f))
	assert.Equal(t, []string{"grant_x.odd", "grant.remove"},
		actions(AuditFilter{Since: t0.Add(time.Minute), Until: t0.Add(3 * time.Minute)}, f))
	assert.Equal(t, []string{"grant.add"},
		actions(AuditFilter{ActionPrefix: "grant", Actor: "user:alice", Until: t0.Add(time.Minute)}, f))
	assert.Empty(t, actions(AuditFilter{ActionPrefix: "nope"}, f))
}

func TestSetPasswordMustChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	u, err := f.s.CreateUser(f.ctx, "dora", "h1", false)
	require.NoError(t, err)
	require.NoError(t, f.s.InTx(f.ctx, func(tx *Tx) error { return tx.SetPassword(f.ctx, u.ID, "h2", true) }))
	got, err := f.s.UserByID(f.ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, "h2", got.PasswordHash)
	assert.True(t, got.MustChangePassword)
	require.NoError(t, f.s.SetPasswordHash(f.ctx, u.ID, "h3"))
	got, err = f.s.UserByID(f.ctx, u.ID)
	require.NoError(t, err)
	assert.False(t, got.MustChangePassword)
}

func TestTxDeletePendingWorkersReturnsIDs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w, err := f.s.RegisterWorker(f.ctx, "tok-a", api.WorkerFacts{Hostname: "a"})
	require.NoError(t, err)
	f.clk.Advance(time.Hour)
	var ids []string
	require.NoError(t, f.s.InTx(f.ctx, func(tx *Tx) error {
		var err error
		ids, err = tx.DeletePendingWorkersUnseen(f.ctx, f.clk.Now().Add(-time.Minute))
		return err
	}))
	assert.Equal(t, []string{w.ID}, ids)
}
