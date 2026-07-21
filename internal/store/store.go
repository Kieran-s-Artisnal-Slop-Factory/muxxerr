// Package store is the gateway's own database: users, sessions, which app
// instances each user has provisioned, an audit trail, and login throttling.
//
// It never touches app data. Each app instance owns a separate SQLite file
// under data/instances/<user>/<app>/, written only by that instance's child
// process — the isolation that makes single-tenant apps safe to multiplex.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

var (
	ErrNotFound      = errors.New("not found")
	ErrUsernameTaken = errors.New("username already taken")
)

// Store wraps the gateway database.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the gateway database at path.
//
// SetMaxOpenConns(1) is deliberate. SQLite allows one writer, and this
// database is tiny and low-traffic — a single connection removes SQLITE_BUSY
// as a failure mode entirely rather than papering over it with a busy_timeout
// and hoping. _txlock=immediate takes the write lock at BEGIN so a read-then-
// write transaction can never fail an upgrade mid-flight.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests and one-off admin queries.
func (s *Store) DB() *sql.DB { return s.db }

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// ---------------------------------------------------------------- users

// User is a gateway account.
type User struct {
	ID             int64
	Username       string
	PasswordHash   string
	PassphraseHash string
	IsAdmin        bool
	IsDisabled     bool
	CreatedAt      time.Time
	CredentialGen  int64
	LastLoginAt    *time.Time
}

const userCols = `id, username, password_hash, passphrase_hash, is_admin, is_disabled, created_at, credential_gen, last_login_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created string
	var last sql.NullString
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PassphraseHash,
		&u.IsAdmin, &u.IsDisabled, &created, &u.CredentialGen, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if last.Valid {
		if t, err := time.Parse(time.RFC3339Nano, last.String); err == nil {
			u.LastLoginAt = &t
		}
	}
	return &u, nil
}

// CreateUser inserts a new account. Callers must have already hashed both
// secrets — this package never sees a plaintext password.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash, passphraseHash string, isAdmin bool) (*User, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, passphrase_hash, is_admin, created_at, credential_gen)
		 VALUES (?, ?, ?, ?, ?, 1)`,
		username, passwordHash, passphraseHash, isAdmin, now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.UserByID(ctx, id)
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// UserByName looks up by username. Usernames are stored lowercase; callers
// should normalise before calling.
func (s *Store) UserByName(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, username))
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// SetCredentials replaces the password and passphrase and bumps
// credential_gen, which invalidates every existing session for this user
// (see SessionUser). Pass an empty passphraseHash to leave it unchanged.
func (s *Store) SetCredentials(ctx context.Context, userID int64, passwordHash, passphraseHash string) error {
	q := `UPDATE users SET password_hash = ?, credential_gen = credential_gen + 1 WHERE id = ?`
	args := []any{passwordHash, userID}
	if passphraseHash != "" {
		q = `UPDATE users SET password_hash = ?, passphrase_hash = ?, credential_gen = credential_gen + 1 WHERE id = ?`
		args = []any{passwordHash, passphraseHash, userID}
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

func (s *Store) SetAdmin(ctx context.Context, userID int64, admin bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET is_admin = ? WHERE id = ?`, admin, userID)
	return err
}

func (s *Store) SetDisabled(ctx context.Context, userID int64, disabled bool) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET is_disabled = ? WHERE id = ?`, disabled, userID); err != nil {
		return err
	}
	if disabled {
		return s.DeleteUserSessions(ctx, userID)
	}
	return nil
}

func (s *Store) TouchLogin(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, now(), userID)
	return err
}

// DeleteUser removes the account and, by cascade, its sessions and instance
// rows. On-disk instance data is the caller's problem — deleting a user's
// databases is a separate, louder decision.
func (s *Store) DeleteUser(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	return err
}

// ------------------------------------------------------------- sessions

// HashToken is the one-way mapping from cookie value to primary key. The
// plaintext token is high-entropy and random, so a fast hash is right here:
// there is nothing to brute-force, and we do this on every request.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Session is a logged-in device.
type Session struct {
	TokenHash string
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
	UserAgent string
	IP        string
}

func (s *Store) CreateSession(ctx context.Context, token string, u *User, ttl time.Duration, userAgent, ip string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, credential_gen, created_at, expires_at, user_agent, ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		HashToken(token), u.ID, u.CredentialGen, now(),
		time.Now().UTC().Add(ttl).Format(time.RFC3339Nano), userAgent, ip)
	return err
}

// SessionUser resolves a cookie token to its user, or ErrNotFound if the
// session is unknown, expired, superseded by a credential change, or belongs
// to a disabled account. The credential_gen join is what makes a password
// reset log out every other device.
func (s *Store) SessionUser(ctx context.Context, token string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+prefixed("u", userCols)+`
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = ?
		    AND s.expires_at > ?
		    AND s.credential_gen = u.credential_gen
		    AND u.is_disabled = 0`,
		HashToken(token), now())
	return scanUser(row)
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, HashToken(token))
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// PurgeExpiredSessions drops rows that can no longer authenticate anything.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// prefixed qualifies a column list with a table alias.
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

// ------------------------------------------------------------ instances

// Instance is one (user, app) pair the user has provisioned.
type Instance struct {
	ID         int64
	UserID     int64
	Username   string
	App        string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// AddInstance provisions an app for a user. It is idempotent: re-adding an
// app the user already has is a no-op rather than an error.
func (s *Store) AddInstance(ctx context.Context, userID int64, app string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO instances (user_id, app, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(user_id, app) DO NOTHING`,
		userID, app, now())
	return err
}

// RemoveInstance forgets the provisioning record. It does NOT delete the
// instance's database — callers decide that explicitly.
func (s *Store) RemoveInstance(ctx context.Context, userID int64, app string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM instances WHERE user_id = ? AND app = ?`, userID, app)
	return err
}

func (s *Store) HasInstance(ctx context.Context, userID int64, app string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM instances WHERE user_id = ? AND app = ?`, userID, app).Scan(&n)
	return n > 0, err
}

func (s *Store) TouchInstance(ctx context.Context, userID int64, app string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE instances SET last_used_at = ? WHERE user_id = ? AND app = ?`, now(), userID, app)
	return err
}

func (s *Store) UserInstances(ctx context.Context, userID int64) ([]Instance, error) {
	return s.queryInstances(ctx,
		`SELECT i.id, i.user_id, u.username, i.app, i.created_at, i.last_used_at
		   FROM instances i JOIN users u ON u.id = i.user_id
		  WHERE i.user_id = ? ORDER BY i.app`, userID)
}

// AllInstances powers the admin overview of who has what.
func (s *Store) AllInstances(ctx context.Context) ([]Instance, error) {
	return s.queryInstances(ctx,
		`SELECT i.id, i.user_id, u.username, i.app, i.created_at, i.last_used_at
		   FROM instances i JOIN users u ON u.id = i.user_id
		  ORDER BY u.username, i.app`)
}

func (s *Store) queryInstances(ctx context.Context, q string, args ...any) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Instance{}
	for rows.Next() {
		var in Instance
		var created string
		var last sql.NullString
		if err := rows.Scan(&in.ID, &in.UserID, &in.Username, &in.App, &created, &last); err != nil {
			return nil, err
		}
		in.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if last.Valid {
			if t, err := time.Parse(time.RFC3339Nano, last.String); err == nil {
				in.LastUsedAt = &t
			}
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------- settings

func (s *Store) Setting(ctx context.Context, key, fallback string) string {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return fallback
	}
	return v
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) BoolSetting(ctx context.Context, key string, fallback bool) bool {
	switch s.Setting(ctx, key, "") {
	case "true":
		return true
	case "false":
		return false
	default:
		return fallback
	}
}

// ------------------------------------------------------------ audit log

type AuditEntry struct {
	At     time.Time
	Actor  string
	Action string
	Target string
	Detail string
	IP     string
}

// Audit records a security-relevant action. Failures are swallowed by
// callers: an audit write must never break the request it describes.
func (s *Store) Audit(ctx context.Context, actor, action, target, detail, ip string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (at, actor, action, target, detail, ip) VALUES (?, ?, ?, ?, ?, ?)`,
		now(), actor, action, target, detail, ip)
	return err
}

func (s *Store) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, COALESCE(actor,''), action, COALESCE(target,''), COALESCE(detail,''), COALESCE(ip,'')
		   FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var at string
		if err := rows.Scan(&at, &e.Actor, &e.Action, &e.Target, &e.Detail, &e.IP); err != nil {
			return nil, err
		}
		e.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------- throttle

// Throttle state for one key. Lockout is exponential in the number of
// consecutive failures, capped, and cleared on success.
type Throttle struct {
	Fails       int
	LockedUntil time.Time
}

// CheckThrottle reports whether key is currently locked out, and for how long.
func (s *Store) CheckThrottle(ctx context.Context, key string) (locked bool, until time.Time, err error) {
	var lockedUntil sql.NullString
	var fails int
	err = s.db.QueryRowContext(ctx,
		`SELECT fails, locked_until FROM throttle WHERE key = ?`, key).Scan(&fails, &lockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, err
	}
	if !lockedUntil.Valid {
		return false, time.Time{}, nil
	}
	t, perr := time.Parse(time.RFC3339Nano, lockedUntil.String)
	if perr != nil || !t.After(time.Now().UTC()) {
		return false, time.Time{}, nil
	}
	return true, t, nil
}

// RecordFailure increments the counter for key and returns the new lockout.
// The first few attempts are free; after that the delay doubles each time,
// which stops online guessing without permanently locking a forgetful user
// out of their own account.
func (s *Store) RecordFailure(ctx context.Context, key string, freeAttempts int, base, max time.Duration) (time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()

	var fails int
	err = tx.QueryRowContext(ctx, `SELECT fails FROM throttle WHERE key = ?`, key).Scan(&fails)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	fails++

	var until time.Time
	if fails > freeAttempts {
		d := base << min(fails-freeAttempts-1, 16)
		if d > max || d <= 0 {
			d = max
		}
		until = time.Now().UTC().Add(d)
	}
	var untilStr any
	if !until.IsZero() {
		untilStr = until.Format(time.RFC3339Nano)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO throttle (key, fails, locked_until, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET fails = excluded.fails, locked_until = excluded.locked_until, updated_at = excluded.updated_at`,
		key, fails, untilStr, now()); err != nil {
		return time.Time{}, err
	}
	return until, tx.Commit()
}

func (s *Store) ClearThrottle(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM throttle WHERE key = ?`, key)
	return err
}

// PurgeStaleThrottles drops counters that have long since stopped mattering.
func (s *Store) PurgeStaleThrottles(ctx context.Context, olderThan time.Duration) error {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM throttle WHERE updated_at < ? AND (locked_until IS NULL OR locked_until < ?)`,
		cutoff, now())
	return err
}
