package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
)

// Client is an API principal. The token itself is never stored.
type Client struct {
	ID         string
	Name       string
	CreatedBy  string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Principal returns the client's grant principal.
func (c *Client) Principal() string { return ClientPrincipal(c.ID) }

// GrantSpec is a role on a scope, in internal scope form ("*", "queue:<id>",
// "worker:<id>").
type GrantSpec struct {
	Role  api.Role
	Scope string
}

// NewClient holds the fields of a client to create. Grants are inserted in
// the same transaction with granted_by = CreatedBy; checking that the creator
// may hand them out is the caller's job.
type NewClient struct {
	Name      string
	TokenHash string
	CreatedBy string
	Grants    []GrantSpec
}

// CreateClient creates a client and its grants atomically.
func (s *Store) CreateClient(ctx context.Context, nc NewClient) (*Client, error) {
	return inTx1(ctx, s, func(tx *Tx) (*Client, error) { return tx.CreateClient(ctx, nc) })
}

// CreateClient is Store.CreateClient inside the transaction.
func (t *Tx) CreateClient(ctx context.Context, nc NewClient) (*Client, error) {
	if nc.Name == "" || nc.TokenHash == "" || nc.CreatedBy == "" {
		return nil, fmt.Errorf("%w: client name, token hash and creator are required", ErrInvalid)
	}
	now := fromMS(toMS(t.s.now()))
	c := &Client{ID: t.s.cfg.NewID(IDClient), Name: nc.Name, CreatedBy: nc.CreatedBy, CreatedAt: now}
	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO clients (id, name, token_hash, created_by, created_at) VALUES (?,?,?,?,?)`,
		c.ID, c.Name, nc.TokenHash, c.CreatedBy, toMS(now))
	if isUnique(err) {
		return nil, fmt.Errorf("create client: %w", ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}
	for _, g := range nc.Grants {
		if err := addGrant(ctx, t.tx, c.Principal(), g.Role, g.Scope, nc.CreatedBy, now); err != nil {
			return nil, err
		}
	}
	return c, nil
}

const clientCols = `id, name, created_by, created_at, last_used_at, revoked_at`

func scanClient(row interface{ Scan(...any) error }) (*Client, error) {
	var c Client
	var created int64
	var used, revoked sql.NullInt64
	if err := row.Scan(&c.ID, &c.Name, &c.CreatedBy, &created, &used, &revoked); err != nil {
		return nil, err
	}
	c.CreatedAt = fromMS(created)
	c.LastUsedAt = optTime(used)
	c.RevokedAt = optTime(revoked)
	return &c, nil
}

// ClientByTokenHash returns the unrevoked client with the given token hash.
func (s *Store) ClientByTokenHash(ctx context.Context, hash string) (*Client, error) {
	c, err := scanClient(s.r.QueryRowContext(ctx,
		`SELECT `+clientCols+` FROM clients WHERE token_hash = ? AND revoked_at IS NULL`, hash))
	if err != nil {
		return nil, notFound(err, "client by token")
	}
	return c, nil
}

// ClientByID returns a client, revoked or not.
func (s *Store) ClientByID(ctx context.Context, id string) (*Client, error) {
	c, err := scanClient(s.r.QueryRowContext(ctx, `SELECT `+clientCols+` FROM clients WHERE id = ?`, id))
	if err != nil {
		return nil, notFound(err, "client "+id)
	}
	return c, nil
}

// ListClients returns all clients, oldest first.
func (s *Store) ListClients(ctx context.Context) ([]Client, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+clientCols+` FROM clients ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list clients: %w", err)
	}
	defer rows.Close()
	var out []Client
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, fmt.Errorf("list clients: %w", err)
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// RevokeClient revokes a client's token. Revoking twice keeps the first time.
func (s *Store) RevokeClient(ctx context.Context, id string) error {
	return s.InTx(ctx, func(tx *Tx) error { return tx.RevokeClient(ctx, id) })
}

// RevokeClient is Store.RevokeClient inside the transaction.
func (t *Tx) RevokeClient(ctx context.Context, id string) error {
	res, err := t.tx.ExecContext(ctx,
		`UPDATE clients SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, toMS(t.s.now()), id)
	return mustAffect(res, err, "revoke client")
}

// TouchClient records that a client just authenticated.
func (s *Store) TouchClient(ctx context.Context, id string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE clients SET last_used_at = ? WHERE id = ?`, toMS(s.now()), id)
		return mustAffect(res, err, "touch client")
	})
}
