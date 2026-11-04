package access

import (
	"context"
	"database/sql"
	"slices"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

type UserGroups struct {
	UserID   string   `json:"user_id"`
	GroupIDs []string `json:"group_ids"`
	ScopeID  string   `json:"scope_id"`
	Version  int64    `json:"version"`
}

func userGroupsInTx(ctx context.Context, tx *sql.Tx, id string) (UserGroups, error) {
	var membership UserGroups
	if err := tx.QueryRowContext(ctx, `SELECT id,scope_id,version FROM users WHERE id=?`, id).Scan(&membership.UserID, &membership.ScopeID, &membership.Version); err != nil {
		return UserGroups{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT group_id FROM user_groups WHERE user_id=? ORDER BY group_id LIMIT 101`, id)
	if err != nil {
		return UserGroups{}, err
	}
	membership.GroupIDs, err = scanGroupIDs(rows)
	return membership, err
}

func (s *Service) UserGroups(ctx context.Context, current auth.Session, id string) (UserGroups, error) {
	tx, _, err := s.adminTx(ctx, current)
	if err != nil {
		return UserGroups{}, err
	}
	defer tx.Rollback()
	return userGroupsInTx(ctx, tx, id)
}

func (s *Service) SetUserGroups(ctx context.Context, current auth.Session, id string, version int64, ids []string) (UserGroups, error) {
	ids, data, err := normalizedGroups(ids)
	if err != nil {
		return UserGroups{}, err
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return UserGroups{}, err
	}
	defer tx.Rollback()
	before, err := userGroupsInTx(ctx, tx, id)
	if err != nil {
		return UserGroups{}, err
	}
	if before.Version != version {
		return UserGroups{}, auth.ErrConflict
	}
	if err := knownGroups(ctx, tx, data, len(ids)); err != nil {
		return UserGroups{}, err
	}
	if slices.Equal(before.GroupIDs, ids) {
		return before, nil
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	if _, err := revokeScopes(ctx, tx, `u.id=?`, []any{id}, now); err != nil {
		return UserGroups{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_groups WHERE user_id=?`, id); err != nil {
		return UserGroups{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_groups(user_id,group_id) SELECT ?,value FROM json_each(?)`, id, data); err != nil {
		return UserGroups{}, err
	}
	membership, err := userGroupsInTx(ctx, tx, id)
	if err != nil {
		return UserGroups{}, err
	}
	if err := store.Change(ctx, tx, membership.ScopeID, "user_groups", id, "upsert", membership.Version, membership, now); err != nil {
		return UserGroups{}, err
	}
	if err := store.Audit(ctx, tx, actor.ID, "user_groups_changed", "user", id, map[string]any{"previous_group_ids": before.GroupIDs, "group_ids": ids, "version": membership.Version}, now); err != nil {
		return UserGroups{}, err
	}
	return membership, tx.Commit()
}
