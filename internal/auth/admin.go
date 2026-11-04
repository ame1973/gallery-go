package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"gallerygo/internal/store"
)

var (
	ErrForbidden        = errors.New("administrator access is required")
	ErrLastAdmin        = errors.New("the last active administrator cannot be removed, disabled, or demoted")
	ErrUsernameTaken    = errors.New("the username is already in use")
	ErrInvalidUser      = errors.New("provide a valid role and at least one editable account field")
	ErrIdempotencyKey   = store.ErrIdempotencyKey
	ErrIdempotencyReuse = store.ErrIdempotencyReuse
	ErrRequestInFlight  = store.ErrRequestInFlight
)

type NewUser struct {
	Username         string
	Password         string
	Role             string
	Active           bool
	DownloadOriginal bool
}

type UserPatch struct {
	Username         *string `json:"username"`
	Role             *string `json:"role"`
	Active           *bool   `json:"active"`
	DownloadOriginal *bool   `json:"download_original"`
}

const userSelect = `SELECT id,username,role,active,scope_id,version,download_original FROM users`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var user User
	err := row.Scan(&user.ID, &user.Username, &user.Role, &user.Active, &user.ScopeID, &user.Version, &user.DownloadOriginal)
	return user, err
}

// RequireAdmin rechecks the actual session and account inside the caller's write
// transaction. A previously authenticated DTO is not an authorization capability.
func (s *Service) RequireAdmin(ctx context.Context, tx *sql.Tx, current Session) (Session, error) {
	current, err := s.currentInTx(ctx, tx, current)
	if err != nil {
		return Session{}, err
	}
	if current.User.Role != "admin" {
		return Session{}, ErrForbidden
	}
	return current, nil
}

func (s *Service) adminNow(ctx context.Context, current Session) (Session, error) {
	current, err := s.Authenticate(ctx, current.Token)
	if err != nil {
		return Session{}, err
	}
	if current.User.Role != "admin" {
		return Session{}, ErrForbidden
	}
	return current, nil
}

func (s *Service) Users(ctx context.Context, current Session, after string, limit int) ([]User, error) {
	if limit < 1 || limit > 101 {
		return nil, ErrInvalidUser
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := s.RequireAdmin(ctx, tx, current); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, userSelect+` WHERE id>? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]User, 0)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, user)
	}
	return items, rows.Err()
}

func (s *Service) GetUser(ctx context.Context, current Session, id string) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	if _, err := s.RequireAdmin(ctx, tx, current); err != nil {
		return User{}, err
	}
	return scanUser(tx.QueryRowContext(ctx, userSelect+` WHERE id=?`, id))
}

func (s *Service) CreateUser(ctx context.Context, current Session, input NewUser, key string) (User, error) {
	current, err := s.adminNow(ctx, current)
	if err != nil {
		return User{}, err
	}
	username, err := NormalizeUsername(input.Username)
	if err != nil {
		return User{}, err
	}
	if input.Role != "admin" && input.Role != "user" {
		return User{}, ErrInvalidUser
	}
	user := User{Username: username, Role: input.Role, Active: input.Active, DownloadOriginal: input.DownloadOriginal}
	request, err := s.prepareUserRequest(ctx, current, key, "create", user, input.Password)
	if err != nil {
		return User{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	current, err = s.RequireAdmin(ctx, tx, current)
	if err != nil {
		return User{}, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	var replay User
	if found, err := request.Replay(ctx, tx, now, &replay); found || err != nil {
		return replay, err
	}
	if err := availableUsername(ctx, tx, username, ""); err != nil {
		return User{}, err
	}
	user.ID, user.ScopeID, user.Version = store.NewID(), token(), 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,active,created_at,updated_at,scope_id,version,download_original) VALUES(?,?,?,?,?,?,?,?,1,?)`, user.ID, user.Username, request.PasswordHash, user.Role, user.Active, now.UnixMilli(), now.UnixMilli(), user.ScopeID, user.DownloadOriginal); err != nil {
		return User{}, err
	}
	if err := userEvent(ctx, tx, current.User.ID, "user_created", user, now); err != nil {
		return User{}, err
	}
	if err := request.Save(ctx, tx, user, now); err != nil {
		return User{}, err
	}
	return user, tx.Commit()
}

func availableUsername(ctx context.Context, tx *sql.Tx, name, exceptID string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE username=? AND id!=?)`, name, exceptID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrUsernameTaken
	}
	return nil
}

func (s *Service) UpdateUser(ctx context.Context, current Session, id string, version int64, patch UserPatch) (User, error) {
	if patch.Username == nil && patch.Role == nil && patch.Active == nil && patch.DownloadOriginal == nil {
		return User{}, ErrInvalidUser
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	current, err = s.RequireAdmin(ctx, tx, current)
	if err != nil {
		return User{}, err
	}
	user, err := versionedUser(ctx, tx, id, version)
	if err != nil {
		return User{}, err
	}
	before := user
	if patch.Username != nil {
		user.Username, err = NormalizeUsername(*patch.Username)
		if err != nil {
			return User{}, err
		}
		if err := availableUsername(ctx, tx, user.Username, id); err != nil {
			return User{}, err
		}
	}
	if patch.Role != nil {
		if *patch.Role != "admin" && *patch.Role != "user" {
			return User{}, ErrInvalidUser
		}
		user.Role = *patch.Role
	}
	if patch.Active != nil {
		user.Active = *patch.Active
	}
	if patch.DownloadOriginal != nil {
		user.DownloadOriginal = *patch.DownloadOriginal
	}
	if user == before {
		return user, nil
	}
	if before.Role == "admin" && before.Active && (user.Role != "admin" || !user.Active) {
		if err := anotherActiveAdmin(ctx, tx, id); err != nil {
			return User{}, err
		}
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	if err := invalidateUser(ctx, tx, &user, now); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET username=?,role=?,active=?,download_original=? WHERE id=?`, user.Username, user.Role, user.Active, user.DownloadOriginal, id); err != nil {
		return User{}, err
	}
	if err := userEvent(ctx, tx, current.User.ID, "user_updated", user, now); err != nil {
		return User{}, err
	}
	return user, tx.Commit()
}

func versionedUser(ctx context.Context, tx *sql.Tx, id string, version int64) (User, error) {
	user, err := scanUser(tx.QueryRowContext(ctx, userSelect+` WHERE id=?`, id))
	if err != nil {
		return User{}, err
	}
	if user.Version != version {
		return User{}, ErrConflict
	}
	return user, nil
}

func anotherActiveAdmin(ctx context.Context, tx *sql.Tx, id string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE role='admin' AND active=1 AND id!=?)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrLastAdmin
	}
	return nil
}

// invalidateUser rotates the authorization scope, revokes every session, and
// leaves a revoke in the old scope for the future authorization-projected sync log.
func invalidateUser(ctx context.Context, tx *sql.Tx, user *User, now time.Time) error {
	oldScope := user.ScopeID
	user.ScopeID, user.Version = token(), user.Version+1
	if _, err := tx.ExecContext(ctx, `UPDATE users SET scope_id=?,version=?,updated_at=? WHERE id=?`, user.ScopeID, user.Version, now.UnixMilli(), user.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now.UnixMilli(), user.ID); err != nil {
		return err
	}
	return store.Change(ctx, tx, oldScope, "scope", user.ID, "revoke", user.Version, map[string]string{"reason": "account_changed"}, now)
}

func userEvent(ctx context.Context, tx *sql.Tx, actor, event string, user User, now time.Time) error {
	if err := store.Change(ctx, tx, user.ScopeID, "user", user.ID, "upsert", user.Version, user, now); err != nil {
		return err
	}
	return store.Audit(ctx, tx, actor, event, "user", user.ID, map[string]any{"username": user.Username, "role": user.Role, "active": user.Active, "download_original": user.DownloadOriginal, "version": user.Version}, now)
}

func (s *Service) ResetPassword(ctx context.Context, current Session, id string, version int64, password, key string) (User, error) {
	current, err := s.adminNow(ctx, current)
	if err != nil {
		return User{}, err
	}
	request, err := s.prepareUserRequest(ctx, current, key, "reset-password", struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}{id, version}, password)
	if err != nil {
		return User{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	current, err = s.RequireAdmin(ctx, tx, current)
	if err != nil {
		return User{}, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	var replay User
	if found, err := request.Replay(ctx, tx, now, &replay); found || err != nil {
		return replay, err
	}
	user, err := versionedUser(ctx, tx, id, version)
	if err != nil {
		return User{}, err
	}
	if err := invalidateUser(ctx, tx, &user, now); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, request.PasswordHash, id); err != nil {
		return User{}, err
	}
	if err := userEvent(ctx, tx, current.User.ID, "password_reset", user, now); err != nil {
		return User{}, err
	}
	if err := request.Save(ctx, tx, user, now); err != nil {
		return User{}, err
	}
	return user, tx.Commit()
}

func (s *Service) DeleteUser(ctx context.Context, current Session, id string, version int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err = s.RequireAdmin(ctx, tx, current)
	if err != nil {
		return err
	}
	user, err := versionedUser(ctx, tx, id, version)
	if err != nil {
		return err
	}
	if user.Role == "admin" && user.Active {
		if err := anotherActiveAdmin(ctx, tx, id); err != nil {
			return err
		}
	}
	// These baseline foreign keys deliberately do not cascade. Delete only the
	// account's application metadata; no source, album, or filesystem row is touched.
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM playback_progress WHERE user_id=? OR playlist_id IN (SELECT id FROM playlists WHERE user_id=?)`, []any{id, id}},
		{`DELETE FROM playlists WHERE user_id=?`, []any{id}},
		{`DELETE FROM sync_operations WHERE user_id=?`, []any{id}},
		{`DELETE FROM users WHERE id=?`, []any{id}},
	} {
		if _, err := tx.ExecContext(ctx, query.sql, query.args...); err != nil {
			return err
		}
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	if err := store.Change(ctx, tx, user.ScopeID, "scope", id, "revoke", user.Version+1, map[string]string{"reason": "account_deleted"}, now); err != nil {
		return err
	}
	if err := store.Change(ctx, tx, user.ScopeID, "user", id, "delete", user.Version+1, map[string]string{"id": id}, now); err != nil {
		return err
	}
	if err := store.Audit(ctx, tx, current.User.ID, "user_deleted", "user", id, map[string]string{"username": user.Username}, now); err != nil {
		return err
	}
	return tx.Commit()
}
