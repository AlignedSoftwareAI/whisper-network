package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// User is a human UI login. The store never sees passwords, only hashes.
type User struct {
	ID                 int64
	Username           string
	PasswordHash       string
	MustChangePassword bool
	DisabledAt         *time.Time
	CreatedAt          time.Time
}

// Principal returns the user's grant principal.
func (u *User) Principal() string { return UserPrincipal(u.ID) }

// CreateUser creates a user. ErrConflict if the username is taken.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, mustChange bool) (*User, error) {
	return inTx1(ctx, s, func(tx *Tx) (*User, error) { return tx.CreateUser(ctx, username, passwordHash, mustChange) })
}

// CreateUser is Store.CreateUser inside the transaction.
func (t *Tx) CreateUser(ctx context.Context, username, passwordHash string, mustChange bool) (*User, error) {
	return t.s.createUser(ctx, t.tx, username, passwordHash, mustChange)
}

func (s *Store) createUser(ctx context.Context, tx *sql.Tx, username, passwordHash string, mustChange bool) (*User, error) {
	if username == "" || passwordHash == "" {
		return nil, fmt.Errorf("%w: username and password hash are required", ErrInvalid)
	}
	now := fromMS(toMS(s.now()))
	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, must_change_password, created_at) VALUES (?,?,?,?)`,
		username, passwordHash, mustChange, toMS(now))
	if isUnique(err) {
		return nil, fmt.Errorf("create user %q: %w", username, ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("create user %q: %w", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create user %q: %w", username, err)
	}
	return &User{ID: id, Username: username, PasswordHash: passwordHash, MustChangePassword: mustChange, CreatedAt: now}, nil
}

const userCols = `id, username, password_hash, must_change_password, disabled_at, created_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var disabled sql.NullInt64
	var created int64
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.MustChangePassword, &disabled, &created); err != nil {
		return nil, err
	}
	u.DisabledAt = optTime(disabled)
	u.CreatedAt = fromMS(created)
	return &u, nil
}

// UserByUsername returns the user with the given username (disabled or not).
func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	u, err := scanUser(s.r.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, username))
	if err != nil {
		return nil, notFound(err, "user "+username)
	}
	return u, nil
}

// UserByID returns the user with the given id (disabled or not).
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	u, err := scanUser(s.r.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if err != nil {
		return nil, notFound(err, fmt.Sprintf("user %d", id))
	}
	return u, nil
}

// ListUsers returns all users ordered by username.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("list users: %w", err)
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// SetPasswordHash replaces a user's password hash and clears
// must_change_password.
func (s *Store) SetPasswordHash(ctx context.Context, id int64, hash string) error {
	return s.InTx(ctx, func(tx *Tx) error { return tx.SetPassword(ctx, id, hash, false) })
}

// SetPassword replaces a user's password hash and sets
// must_change_password to mustChange (true when an administrator chose or
// generated the password, so the user must pick their own at next login).
func (t *Tx) SetPassword(ctx context.Context, id int64, hash string, mustChange bool) error {
	if hash == "" {
		return fmt.Errorf("%w: empty password hash", ErrInvalid)
	}
	res, err := t.tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, must_change_password = ? WHERE id = ?`, hash, mustChange, id)
	return mustAffect(res, err, "set password")
}

// DisableUser disables a user and deletes their sessions. Disabling an
// already-disabled user keeps the original timestamp.
func (s *Store) DisableUser(ctx context.Context, id int64) error {
	return s.InTx(ctx, func(tx *Tx) error { return tx.DisableUser(ctx, id) })
}

// DisableUser is Store.DisableUser inside the transaction.
func (t *Tx) DisableUser(ctx context.Context, id int64) error {
	res, err := t.tx.ExecContext(ctx,
		`UPDATE users SET disabled_at = COALESCE(disabled_at, ?) WHERE id = ?`, toMS(t.s.now()), id)
	if err := mustAffect(res, err, "disable user"); err != nil {
		return err
	}
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("disable user: %w", err)
	}
	return nil
}

// Session is a UI login session. ID is the sha256 of the cookie value.
type Session struct {
	ID        string
	UserID    int64
	CSRF      string
	ExpiresAt time.Time
}

// CreateSession stores a session. ErrConflict if the id exists.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	return s.InTx(ctx, func(tx *Tx) error { return tx.CreateSession(ctx, sess) })
}

// CreateSession is Store.CreateSession inside the transaction.
func (t *Tx) CreateSession(ctx context.Context, sess Session) error {
	if sess.ID == "" || sess.CSRF == "" {
		return fmt.Errorf("%w: session id and csrf are required", ErrInvalid)
	}
	_, err := t.tx.ExecContext(ctx, `INSERT INTO sessions (id, user_id, csrf, expires_at) VALUES (?,?,?,?)`,
		sess.ID, sess.UserID, sess.CSRF, toMS(sess.ExpiresAt))
	if isUnique(err) {
		return fmt.Errorf("create session: %w", ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// ErrCredentialsChanged is returned by ConfirmCredentials when the user's
// password changed, or the user was disabled or deleted, after the
// password was verified.
var ErrCredentialsChanged = errors.New("store: credentials changed since verification")

// ConfirmCredentials checks, inside the transaction, that user id still has
// exactly passwordHash (the hash a login just verified against) and is not
// disabled. Creating a session in the same transaction closes the window
// in which a password reset or disable could land between verification and
// the session's creation, leaving a session made with the old password.
func (t *Tx) ConfirmCredentials(ctx context.Context, id int64, passwordHash string) error {
	var hash string
	var disabled sql.NullInt64
	err := t.tx.QueryRowContext(ctx, `SELECT password_hash, disabled_at FROM users WHERE id = ?`, id).Scan(&hash, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCredentialsChanged
	}
	if err != nil {
		return fmt.Errorf("confirm credentials: %w", err)
	}
	if disabled.Valid || hash != passwordHash {
		return ErrCredentialsChanged
	}
	return nil
}

// Session returns an unexpired session whose user is not disabled.
func (s *Store) Session(ctx context.Context, id string) (*Session, error) {
	var sess Session
	var exp int64
	err := s.r.QueryRowContext(ctx, `
		SELECT s.id, s.user_id, s.csrf, s.expires_at FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = ? AND s.expires_at > ? AND u.disabled_at IS NULL`, id, toMS(s.now())).
		Scan(&sess.ID, &sess.UserID, &sess.CSRF, &exp)
	if err != nil {
		return nil, notFound(err, "session")
	}
	sess.ExpiresAt = fromMS(exp)
	return &sess, nil
}

// DeleteSession deletes a session. Deleting a missing session is not an error.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	return s.InTx(ctx, func(tx *Tx) error {
		_, err := tx.DeleteSession(ctx, id)
		return err
	})
}

// DeleteSession deletes a session and reports whether it existed.
func (t *Tx) DeleteSession(ctx context.Context, id string) (bool, error) {
	res, err := t.tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete session: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteUserSessions deletes every session of a user (e.g. on password change).
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	return s.InTx(ctx, func(tx *Tx) error {
		_, err := tx.DeleteUserSessions(ctx, userID)
		return err
	})
}

// DeleteUserSessions deletes every session of a user and returns how many
// there were.
func (t *Tx) DeleteUserSessions(ctx context.Context, userID int64) (int64, error) {
	res, err := t.tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, fmt.Errorf("delete user sessions: %w", err)
	}
	return res.RowsAffected()
}

// DeleteExpiredSessions deletes sessions past their expiry and returns how many.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	return inTx1(ctx, s, func(tx *Tx) (int64, error) { return tx.DeleteExpiredSessions(ctx) })
}

// DeleteExpiredSessions is Store.DeleteExpiredSessions inside the transaction.
func (t *Tx) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := t.tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, toMS(t.s.now()))
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return res.RowsAffected()
}
