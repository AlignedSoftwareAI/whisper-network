package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

func TestMigrationsIdempotentOnReopen(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	v, err := f.s.SchemaVersion(f.ctx)
	require.NoError(t, err)
	ms, err := loadMigrations()
	require.NoError(t, err)
	assert.Equal(t, len(ms), v)
	f.queue("kept", 7)
	require.NoError(t, f.s.Close())

	s2 := f.open()
	v2, err := s2.SchemaVersion(f.ctx)
	require.NoError(t, err)
	assert.Equal(t, v, v2)
	q, err := s2.QueueByName(f.ctx, "kept")
	require.NoError(t, err)
	assert.Equal(t, 7, q.Weight)

	var journal string
	require.NoError(t, s2.r.QueryRowContext(f.ctx, `PRAGMA journal_mode`).Scan(&journal))
	assert.Equal(t, "wal", journal)
	var fk int
	require.NoError(t, s2.w.QueryRowContext(f.ctx, `PRAGMA foreign_keys`).Scan(&fk))
	assert.Equal(t, 1, fk)
}

func TestOpenRejectsBadConfig(t *testing.T) {
	t.Parallel()
	_, err := Open(t.Context(), Config{})
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestBootstrap(t *testing.T) {
	t.Parallel()
	f := newFixture(t) // bootstraps once with "hash"
	created, err := f.s.Bootstrap(f.ctx, "other")
	require.NoError(t, err)
	assert.False(t, created, "second bootstrap leaves admin alone")

	u, err := f.s.UserByUsername(f.ctx, AdminUsername)
	require.NoError(t, err)
	assert.Equal(t, "hash", u.PasswordHash)
	assert.True(t, u.MustChangePassword)
	gs, err := f.s.GrantsFor(f.ctx, u.Principal())
	require.NoError(t, err)
	require.Len(t, gs, 1)
	assert.Equal(t, api.RoleAdmin, gs[0].Role)
	assert.Equal(t, ScopeAll, gs[0].Scope)

	require.NoError(t, f.s.SetPasswordHash(f.ctx, u.ID, "new"))
	u, err = f.s.UserByID(f.ctx, u.ID)
	require.NoError(t, err)
	assert.False(t, u.MustChangePassword)
	assert.Equal(t, "new", u.PasswordHash)
}

func TestUsersAndSessions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	u, err := f.s.CreateUser(f.ctx, "alice", "h", false)
	require.NoError(t, err)
	_, err = f.s.CreateUser(f.ctx, "alice", "h", false)
	assert.ErrorIs(t, err, ErrConflict)

	exp := f.clk.Now().Add(time.Hour)
	require.NoError(t, f.s.CreateSession(f.ctx, Session{ID: "s1", UserID: u.ID, CSRF: "c", ExpiresAt: exp}))
	sess, err := f.s.Session(f.ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, exp, sess.ExpiresAt)

	f.clk.Advance(2 * time.Hour)
	_, err = f.s.Session(f.ctx, "s1")
	assert.ErrorIs(t, err, ErrNotFound, "expired")
	n, err := f.s.DeleteExpiredSessions(f.ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	require.NoError(t, f.s.CreateSession(f.ctx, Session{ID: "s2", UserID: u.ID, CSRF: "c", ExpiresAt: f.clk.Now().Add(time.Hour)}))
	require.NoError(t, f.s.DisableUser(f.ctx, u.ID))
	_, err = f.s.Session(f.ctx, "s2")
	assert.ErrorIs(t, err, ErrNotFound, "disabling deletes sessions")
	u, err = f.s.UserByID(f.ctx, u.ID)
	require.NoError(t, err)
	assert.NotNil(t, u.DisabledAt)

	users, err := f.s.ListUsers(f.ctx)
	require.NoError(t, err)
	assert.Len(t, users, 2)
}

func TestClientsAndGrants(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scope := QueueScope(f.pubID)
	c, err := f.s.CreateClient(f.ctx, NewClient{Name: "harness", TokenHash: "th", CreatedBy: "user:1",
		Grants: []GrantSpec{{Role: api.RoleSubmit, Scope: scope}}})
	require.NoError(t, err)

	got, err := f.s.ClientByTokenHash(f.ctx, "th")
	require.NoError(t, err)
	assert.Equal(t, c.ID, got.ID)
	require.NoError(t, f.s.TouchClient(f.ctx, c.ID))
	got, err = f.s.ClientByID(f.ctx, c.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.LastUsedAt)

	ps, err := f.s.PrincipalsWithRole(f.ctx, api.RoleSubmit, scope)
	require.NoError(t, err)
	assert.Equal(t, []string{c.Principal()}, ps)

	require.NoError(t, f.s.AddGrant(f.ctx, c.Principal(), api.RoleSubmit, scope, "user:2"), "re-grant is a no-op")
	gs, err := f.s.GrantsFor(f.ctx, c.Principal())
	require.NoError(t, err)
	require.Len(t, gs, 1)
	assert.Equal(t, "user:1", gs[0].GrantedBy)

	_, err = f.s.RemoveGrant(f.ctx, c.Principal(), api.RoleSubmit, scope)
	require.NoError(t, err)
	_, err = f.s.RemoveGrant(f.ctx, c.Principal(), api.RoleSubmit, scope)
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, f.s.RevokeClient(f.ctx, c.ID))
	_, err = f.s.ClientByTokenHash(f.ctx, "th")
	assert.ErrorIs(t, err, ErrNotFound)
	cs, err := f.s.ListClients(f.ctx)
	require.NoError(t, err)
	require.Len(t, cs, 1)
	assert.NotNil(t, cs[0].RevokedAt)

	id, err := ParseQueueScope(scope)
	require.NoError(t, err)
	assert.Equal(t, f.pubID, id)
	_, err = ParseQueueScope("*")
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestQueues(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.s.CreateQueue(f.ctx, NewQueue{Name: PublicQueue})
	assert.ErrorIs(t, err, ErrConflict)
	_, err = f.s.CreateQueue(f.ctx, NewQueue{Name: "Bad Name"})
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = f.s.CreateQueue(f.ctx, NewQueue{Name: "x", Weight: 10001})
	assert.ErrorIs(t, err, ErrInvalid)

	id := f.queue("meetings", 0)
	w, d, m := 300, "council", int64(1<<30)
	q, err := f.s.PatchQueue(f.ctx, id, api.QueuePatch{Weight: &w, Description: &d, MaxUploadBytes: &m})
	require.NoError(t, err)
	assert.Equal(t, api.Queue{Name: "meetings", Description: d, Weight: w, MaxUploadBytes: m}, q.API())
	bad := 0
	_, err = f.s.PatchQueue(f.ctx, id, api.QueuePatch{Weight: &bad})
	assert.ErrorIs(t, err, ErrInvalid)

	assert.ErrorIs(t, f.s.RenameQueue(f.ctx, id, PublicQueue), ErrConflict)
	require.NoError(t, f.s.RenameQueue(f.ctx, id, "council"))
	q, err = f.s.QueueByID(f.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "council", q.Name)
	assert.ErrorIs(t, f.s.RenameQueue(f.ctx, 999, "nope"), ErrNotFound)

	qs, err := f.s.ListQueues(f.ctx)
	require.NoError(t, err)
	assert.Len(t, qs, 2)
}

func TestAudit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for i := range 5 {
		_, err := f.s.AppendAudit(f.ctx, AuditEntry{Actor: "user:1", Action: "grant", Target: string(rune('a' + i))})
		require.NoError(t, err)
	}
	page, err := f.s.ListAudit(f.ctx, 0, 3)
	require.NoError(t, err)
	require.Len(t, page, 3)
	assert.Equal(t, "e", page[0].Target, "newest first")
	page2, err := f.s.ListAudit(f.ctx, page[2].ID, 3)
	require.NoError(t, err)
	require.Len(t, page2, 3, "two entries plus the bootstrap's admin")
	assert.Equal(t, "a", page2[1].Target)
	assert.Equal(t, "user.create", page2[2].Action, "bootstrap audits the admin it creates")
	assert.Equal(t, SystemActor, page2[2].Actor)
	_, err = f.s.AppendAudit(f.ctx, AuditEntry{})
	assert.ErrorIs(t, err, ErrInvalid)
}
