package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"gallerygo/internal/store"
	"golang.org/x/crypto/bcrypt"
)

type Policy struct {
	Idle     time.Duration `yaml:"session_idle_timeout"`
	Absolute time.Duration `yaml:"session_absolute_timeout"`
}

func DefaultPolicy() Policy { return Policy{Idle: 24 * time.Hour, Absolute: 30 * 24 * time.Hour} }

func (p Policy) Validate() error {
	if p.Idle < time.Minute || p.Idle > 24*time.Hour || p.Absolute < p.Idle || p.Absolute > 30*24*time.Hour {
		return errors.New("session idle must be 1m..24h and absolute must be idle..720h")
	}
	return nil
}

type User struct {
	ID               string `json:"id"`
	Username         string `json:"username"`
	Role             string `json:"role"`
	Active           bool   `json:"active"`
	ScopeID          string `json:"scope_id"`
	Version          int64  `json:"version"`
	DownloadOriginal bool   `json:"download_original"`
}

type Session struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	UserAgent  string    `json:"user_agent"`
	IPAddress  string    `json:"ip_address"`
	Current    bool      `json:"current"`
	User       User      `json:"-"`
	Token      string    `json:"-"`
	csrfHash   string
}

func (s Session) CSRFToken() string { return csrfToken(s.Token) }
func (s Session) ValidCSRF(value string) bool {
	return validToken(value) && subtle.ConstantTimeCompare([]byte(s.csrfHash), []byte(digest(value))) == 1
}

type Service struct {
	db            *sql.DB
	policy        Policy
	now           func() time.Time
	dummyHash     string
	passwordSlots chan struct{}
}

var dummyPasswordHash = sync.OnceValues(func() (string, error) { return HashPassword(token()) })

func New(db *sql.DB, policy Policy) (*Service, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	// Missing users must do the same password work as existing users.
	dummy, err := dummyPasswordHash()
	if err != nil {
		return nil, err
	}
	return &Service{db: db, policy: policy, now: time.Now, dummyHash: dummy, passwordSlots: make(chan struct{}, 4)}, nil
}

func (s *Service) passwordSlot(ctx context.Context) (func(), error) {
	// ponytail: four expensive hashes per process; tune only with measured auth load.
	select {
	case s.passwordSlots <- struct{}{}:
		return func() { <-s.passwordSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) Bootstrap(ctx context.Context, username, password string) (User, error) {
	username, err := NormalizeUsername(username)
	if err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM installation)+(SELECT COUNT(*) FROM users)`).Scan(&count); err != nil {
		return User{}, err
	}
	if count != 0 {
		return User{}, ErrBootstrapped
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	user := User{ID: store.NewID(), Username: username, Role: "admin", Active: true, ScopeID: token(), Version: 1, DownloadOriginal: true}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,active,created_at,updated_at,scope_id,version,download_original) VALUES(?,?,?,'admin',1,?,?,?,1,1)`, user.ID, username, hash, now.UnixMilli(), now.UnixMilli(), user.ScopeID); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO installation(id,bootstrapped_at) VALUES(1,?)`, now.UnixMilli()); err != nil {
		return User{}, err
	}
	if err := store.Change(ctx, tx, user.ScopeID, "user", user.ID, "upsert", 1, user, now); err != nil {
		return User{}, err
	}
	if err := store.Audit(ctx, tx, user.ID, "admin_bootstrapped", "user", user.ID, map[string]string{"username": username}, now); err != nil {
		return User{}, err
	}
	return user, tx.Commit()
}

func (s *Service) Login(ctx context.Context, username, password, ip, userAgent, oldToken string) (Session, error) {
	username, err := NormalizeUsername(username)
	if err != nil {
		return Session{}, ErrCredentials
	}
	if err := s.reserveLogin(ctx, username, ip); err != nil {
		return Session{}, err
	}
	release, err := s.passwordSlot(ctx)
	if err != nil {
		return Session{}, err
	}
	defer release()
	var user User
	var hash string
	err = s.db.QueryRowContext(ctx, `SELECT id,username,password_hash,role,active,scope_id,version,download_original FROM users WHERE username=?`, username).Scan(&user.ID, &user.Username, &hash, &user.Role, &user.Active, &user.ScopeID, &user.Version, &user.DownloadOriginal)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Session{}, err
	}
	found := err == nil
	if !found {
		hash = s.dummyHash
	}
	candidate := password
	if len(candidate) > 72 {
		candidate = "invalid oversized credential"
	}
	match := bcrypt.CompareHashAndPassword([]byte(hash), []byte(candidate)) == nil && len(password) <= 72
	if !found || !match || !user.Active {
		if err := s.loginFailure(ctx, user.ID, username, ip); err != nil {
			return Session{}, err
		}
		return Session{}, ErrCredentials
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	// Recheck the password revision and active state after expensive hashing.
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT version FROM users WHERE id=? AND active=1 AND password_hash=?`, user.ID, hash).Scan(&version); errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrCredentials
	} else if err != nil {
		return Session{}, err
	}
	if version != user.Version {
		return Session{}, ErrConflict
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	session := Session{ID: store.NewID(), Token: token(), CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(s.policy.Absolute), UserAgent: userAgent, IPAddress: ip, User: user, Current: true}
	session.csrfHash = digest(session.CSRFToken())
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id_hash,id,user_id,csrf_hash,created_at,last_seen_at,expires_at,user_agent,ip_address) VALUES(?,?,?,?,?,?,?,?,?)`, digest(session.Token), session.ID, user.ID, session.csrfHash, now.UnixMilli(), now.UnixMilli(), session.ExpiresAt.UnixMilli(), userAgent, ip); err != nil {
		return Session{}, err
	}
	if validToken(oldToken) {
		// Logging into another account rotates the browser's previous session too.
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE id_hash=? AND revoked_at IS NULL`, now.UnixMilli(), digest(oldToken)); err != nil {
			return Session{}, err
		}
	}
	if err := s.successfulLogin(ctx, tx, username, ip, now); err != nil {
		return Session{}, err
	}
	if err := store.Change(ctx, tx, user.ScopeID, "session", session.ID, "upsert", 1, map[string]string{"user_id": user.ID}, now); err != nil {
		return Session{}, err
	}
	if err := store.Audit(ctx, tx, user.ID, "login_succeeded", "session", session.ID, map[string]string{"ip_address": ip, "user_agent": userAgent}, now); err != nil {
		return Session{}, err
	}
	return session, tx.Commit()
}

const sessionSelect = `SELECT s.id,s.created_at,s.last_seen_at,s.expires_at,s.user_agent,s.ip_address,s.csrf_hash,
 u.id,u.username,u.role,u.active,u.scope_id,u.version,u.download_original
 FROM sessions s JOIN users u ON u.id=s.user_id
 WHERE s.id_hash=? AND s.revoked_at IS NULL AND s.expires_at>? AND s.last_seen_at>? AND u.active=1`

func scanSession(row *sql.Row, token string) (Session, error) {
	var session Session
	var created, last, expires int64
	err := row.Scan(&session.ID, &created, &last, &expires, &session.UserAgent, &session.IPAddress, &session.csrfHash, &session.User.ID, &session.User.Username, &session.User.Role, &session.User.Active, &session.User.ScopeID, &session.User.Version, &session.User.DownloadOriginal)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}
	session.Token = token
	session.Current = true
	session.CreatedAt = time.UnixMilli(created).UTC()
	session.LastSeenAt = time.UnixMilli(last).UTC()
	session.ExpiresAt = time.UnixMilli(expires).UTC()
	return session, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if !validToken(token) {
		return Session{}, ErrUnauthenticated
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	session, err := scanSession(s.db.QueryRowContext(ctx, sessionSelect, digest(token), now.UnixMilli(), now.Add(-s.policy.Idle).UnixMilli()), token)
	if err != nil {
		return Session{}, err
	}
	if now.After(session.LastSeenAt) {
		result, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at=? WHERE id_hash=? AND revoked_at IS NULL AND expires_at>? AND last_seen_at>? AND EXISTS(SELECT 1 FROM users WHERE users.id=sessions.user_id AND active=1)`, now.UnixMilli(), digest(token), now.UnixMilli(), now.Add(-s.policy.Idle).UnixMilli())
		if err != nil {
			return Session{}, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return Session{}, err
		}
		if n != 1 {
			return Session{}, ErrUnauthenticated
		}
		session.LastSeenAt = now
	}
	return session, nil
}

func (s *Service) currentInTx(ctx context.Context, tx *sql.Tx, current Session) (Session, error) {
	now := s.now().UTC().Truncate(time.Millisecond)
	return scanSession(tx.QueryRowContext(ctx, sessionSelect, digest(current.Token), now.UnixMilli(), now.Add(-s.policy.Idle).UnixMilli()), current.Token)
}

func (s *Service) Revoke(ctx context.Context, current Session, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err = s.currentInTx(ctx, tx, current)
	if err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL`, now.UnixMilli(), id, current.User.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	if err := store.Change(ctx, tx, current.User.ScopeID, "session", id, "delete", 1, map[string]string{"user_id": current.User.ID}, now); err != nil {
		return err
	}
	if err := store.Audit(ctx, tx, current.User.ID, "session_revoked", "session", id, map[string]bool{"current": id == current.ID}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) Sessions(ctx context.Context, current Session, after string, limit int) ([]Session, error) {
	if limit < 1 || limit > 101 {
		return nil, errors.New("invalid session page limit")
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	rows, err := s.db.QueryContext(ctx, `SELECT id,created_at,last_seen_at,expires_at,user_agent,ip_address FROM sessions WHERE user_id=? AND revoked_at IS NULL AND expires_at>? AND last_seen_at>? AND id>? ORDER BY id LIMIT ?`, current.User.ID, now.UnixMilli(), now.Add(-s.policy.Idle).UnixMilli(), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Session, 0)
	for rows.Next() {
		var item Session
		var created, last, expires int64
		if err := rows.Scan(&item.ID, &created, &last, &expires, &item.UserAgent, &item.IPAddress); err != nil {
			return nil, err
		}
		item.CreatedAt = time.UnixMilli(created).UTC()
		item.LastSeenAt = time.UnixMilli(last).UTC()
		item.ExpiresAt = time.UnixMilli(expires).UTC()
		item.Current = item.ID == current.ID
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) ChangePassword(ctx context.Context, current Session, oldPassword, newPassword string) (Session, error) {
	if err := s.reserveLogin(ctx, current.User.Username, current.IPAddress); err != nil {
		return Session{}, err
	}
	release, err := s.passwordSlot(ctx)
	if err != nil {
		return Session{}, err
	}
	defer release()
	var oldHash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=? AND active=1`, current.User.ID).Scan(&oldHash); errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrUnauthenticated
	} else if err != nil {
		return Session{}, err
	}
	if len(oldPassword) > 72 || bcrypt.CompareHashAndPassword([]byte(oldHash), []byte(oldPassword)) != nil {
		if err := s.loginFailure(ctx, current.User.ID, current.User.Username, current.IPAddress); err != nil {
			return Session{}, err
		}
		return Session{}, ErrCredentials
	}
	newHash, err := HashPassword(newPassword)
	if err != nil {
		return Session{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	current, err = s.currentInTx(ctx, tx, current)
	if err != nil {
		return Session{}, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	oldToken := current.Token
	current.Token = token()
	current.csrfHash = digest(current.CSRFToken())
	current.User.Version++
	current.User.ScopeID = token()
	current.LastSeenAt = now
	result, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=?,scope_id=?,version=version+1,updated_at=? WHERE id=? AND password_hash=? AND active=1`, newHash, current.User.ScopeID, now.UnixMilli(), current.User.ID, oldHash)
	if err != nil {
		return Session{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Session{}, err
	}
	if n != 1 {
		return Session{}, ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND id!=? AND revoked_at IS NULL`, now.UnixMilli(), current.User.ID, current.ID); err != nil {
		return Session{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET id_hash=?,csrf_hash=?,last_seen_at=? WHERE id_hash=?`, digest(current.Token), current.csrfHash, now.UnixMilli(), digest(oldToken)); err != nil {
		return Session{}, err
	}
	if err := s.successfulLogin(ctx, tx, current.User.Username, current.IPAddress, now); err != nil {
		return Session{}, err
	}
	if err := store.Change(ctx, tx, current.User.ScopeID, "user", current.User.ID, "upsert", current.User.Version, current.User, now); err != nil {
		return Session{}, err
	}
	if err := store.Audit(ctx, tx, current.User.ID, "password_changed", "user", current.User.ID, map[string]bool{"other_sessions_revoked": true}, now); err != nil {
		return Session{}, err
	}
	return current, tx.Commit()
}

func (s Session) String() string { return fmt.Sprintf("session %s for user %s", s.ID, s.User.ID) }
