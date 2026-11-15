package store

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/auth"
)

// TestMigrationReadAll: upgrading a database from before read_all (schema
// 4) gives every view_all and queue_admin grant on a queue a matching
// read_all grant, so those principals keep the transcript access they had.
// Other grants are untouched, and an existing read_all is not duplicated.
func TestMigrationReadAll(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	other := f.queue("other", 1)
	pub, oth := QueueScope(f.pubID), QueueScope(other)
	type row struct {
		principal string
		role      api.Role
		scope     string
	}
	before := []row{
		{"client:viewer", api.RoleViewAll, pub},
		{"user:qa", api.RoleQueueAdmin, oth},
		{"client:both", api.RoleViewAll, pub},
		{"client:both", api.RoleReadAll, pub}, // already migrated by hand
		{"client:submitter", api.RoleSubmit, pub},
		{"client:deleter", api.RoleDeleteAll, pub},
		{"user:root", api.RoleAdmin, ScopeAll},
		{"worker:w", api.RoleProcess, pub},
	}
	for _, r := range before {
		require.NoError(t, f.s.AddGrant(f.ctx, r.principal, r.role, r.scope, "user:granter"))
	}

	// Rewind to the schema before read_all (version 4) and reopen: 0005
	// runs again on these rows exactly as it would on an upgraded
	// deployment. The read_all migration is data-only; 0006 then re-adds
	// leases.heartbeat_at and 0007 the failure streak table and
	// workers.pause_reason, so those are dropped first.
	_, err := f.s.w.ExecContext(f.ctx, `
		ALTER TABLE leases DROP COLUMN heartbeat_at;
		DROP TABLE worker_failure_streak;
		ALTER TABLE workers DROP COLUMN pause_reason;
		PRAGMA user_version = 4`)
	require.NoError(t, err)
	require.NoError(t, f.s.Close())
	s := f.open()
	v, err := s.SchemaVersion(f.ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, v, 7)

	held := func(principal string) []api.Grant {
		gs, err := s.GrantsFor(f.ctx, principal)
		require.NoError(t, err)
		out := make([]api.Grant, len(gs))
		for i, g := range gs {
			out[i] = api.Grant{Role: g.Role, Scope: g.Scope}
			if g.Role == api.RoleReadAll {
				assert.Equal(t, "user:granter", g.GrantedBy, "attributed to the original granter")
			}
		}
		return out
	}
	assert.ElementsMatch(t, []api.Grant{{Role: api.RoleViewAll, Scope: pub}, {Role: api.RoleReadAll, Scope: pub}},
		held("client:viewer"))
	assert.ElementsMatch(t, []api.Grant{{Role: api.RoleQueueAdmin, Scope: oth}, {Role: api.RoleReadAll, Scope: oth}},
		held("user:qa"))
	assert.ElementsMatch(t, []api.Grant{{Role: api.RoleViewAll, Scope: pub}, {Role: api.RoleReadAll, Scope: pub}},
		held("client:both"))
	assert.Equal(t, []api.Grant{{Role: api.RoleSubmit, Scope: pub}}, held("client:submitter"))
	assert.Equal(t, []api.Grant{{Role: api.RoleDeleteAll, Scope: pub}}, held("client:deleter"))
	assert.Equal(t, []api.Grant{{Role: api.RoleAdmin, Scope: ScopeAll}}, held("user:root"))
	assert.Equal(t, []api.Grant{{Role: api.RoleProcess, Scope: pub}}, held("worker:w"))

	// The migrated principals can still read someone else's transcript.
	// Stored scopes name queues by id; auth only compares the two, so the
	// id serves as the queue name here.
	foreign := func(id int64) auth.Resource {
		return auth.Resource{Queue: strconv.FormatInt(id, 10), OwnerID: "client:someone"}
	}
	viewer := auth.Principal{Kind: auth.KindClient, ID: "viewer", Grants: held("client:viewer")}
	assert.True(t, auth.Can(viewer, auth.ActDownloadTranscript, foreign(f.pubID)))
	assert.True(t, auth.Can(viewer, auth.ActViewJobContent, foreign(f.pubID)))
	qa := auth.Principal{Kind: auth.KindUser, ID: "qa", Grants: held("user:qa")}
	assert.True(t, auth.Can(qa, auth.ActDownloadTranscript, foreign(other)))
	assert.False(t, auth.Can(qa, auth.ActDownloadTranscript, foreign(f.pubID)), "only on its own queue")
}
