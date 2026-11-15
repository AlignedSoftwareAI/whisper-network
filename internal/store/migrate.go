package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one numbered schema step; version N is file NNNN_*.sql.
type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: name must be NNNN_description.sql", e.Name())
		}
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", e.Name(), err)
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %q: expected version %d", m.name, i+1)
		}
	}
	return out, nil
}

// migrate applies every migration newer than PRAGMA user_version, each in its
// own transaction together with the user_version bump. The version is
// re-read inside each (BEGIN IMMEDIATE) transaction, so two processes
// opening a fresh database at once (the server and the admin command)
// cannot both apply the same step.
func (s *Store) migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range ms {
		if err := retryBusy(ctx, DefaultBusyTimeout, func() error { return s.applyMigration(ctx, m, len(ms)) }); err != nil {
			return err
		}
	}
	return nil
}

// retryBusy runs op until it returns something other than SQLITE_BUSY or
// limit has passed. It exists for one case the busy_timeout handler does not
// cover: while two connections create the same database file at once, one
// of them switches it from rollback to WAL mode, and the other's next BEGIN
// fails with SQLITE_BUSY immediately (SQLite does not invoke the busy handler
// there). The failure is transient; a retry a few milliseconds later sees
// the file in WAL mode and proceeds.
func retryBusy(ctx context.Context, limit time.Duration, op func() error) error {
	deadline := time.Now().Add(limit)
	delay := 2 * time.Millisecond
	for {
		err := op()
		if !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		delay = min(2*delay, 100*time.Millisecond)
	}
}

func (s *Store) applyMigration(ctx context.Context, m migration, latest int) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %s: begin: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after commit
	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if current > latest {
		return fmt.Errorf("database schema version %d is newer than this binary (%d)", current, latest)
	}
	if current >= m.version {
		return nil
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("migration %s: set version: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: commit: %w", m.name, err)
	}
	return nil
}

// SchemaVersion returns the applied schema version (PRAGMA user_version).
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	if err := s.r.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return v, nil
}
