package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

type Group struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

func scanGroup(row interface{ Scan(...any) error }) (Group, error) {
	var group Group
	var created int64
	err := row.Scan(&group.ID, &group.Name, &group.Version, &created)
	group.CreatedAt = time.UnixMilli(created).UTC()
	return group, err
}

func (s *Service) Groups(ctx context.Context, current auth.Session, after string, limit int) ([]Group, error) {
	if !pageLimit(limit) {
		return nil, ErrGroupSet
	}
	tx, _, err := s.adminTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,name,version,created_at FROM permission_groups WHERE id>? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]Group, 0)
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Service) Group(ctx context.Context, current auth.Session, id string) (Group, error) {
	tx, _, err := s.adminTx(ctx, current)
	if err != nil {
		return Group{}, err
	}
	defer tx.Rollback()
	return scanGroup(tx.QueryRowContext(ctx, `SELECT id,name,version,created_at FROM permission_groups WHERE id=?`, id))
}

func (s *Service) CreateGroup(ctx context.Context, current auth.Session, name, key string) (Group, error) {
	name, err := normalizedName(name)
	if err != nil {
		return Group{}, err
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return Group{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(map[string]string{"name": name})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "group.create", Metadata: string(data)}
	var group Group
	if found, err := command.Replay(ctx, tx, now, &group); found || err != nil {
		return group, err
	}
	if err := groupNameAvailable(ctx, tx, name, ""); err != nil {
		return Group{}, err
	}
	group = Group{ID: store.NewID(), Name: name, Version: 1, CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO permission_groups(id,name,created_at,version) VALUES(?,?,?,1)`, group.ID, name, now.UnixMilli()); err != nil {
		return Group{}, err
	}
	if err := store.Change(ctx, tx, "system", "group", group.ID, "upsert", 1, group, now); err != nil {
		return Group{}, err
	}
	if err := store.Audit(ctx, tx, actor.ID, "group_created", "group", group.ID, map[string]string{"name": name}, now); err != nil {
		return Group{}, err
	}
	if err := command.Save(ctx, tx, group, now); err != nil {
		return Group{}, err
	}
	return group, tx.Commit()
}

func groupNameAvailable(ctx context.Context, tx *sql.Tx, name, exceptID string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM permission_groups WHERE name=? AND id!=?)`, name, exceptID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrNameTaken
	}
	return nil
}

func (s *Service) RenameGroup(ctx context.Context, current auth.Session, id string, version int64, name string) (Group, error) {
	name, err := normalizedName(name)
	if err != nil {
		return Group{}, err
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return Group{}, err
	}
	defer tx.Rollback()
	group, err := scanGroup(tx.QueryRowContext(ctx, `SELECT id,name,version,created_at FROM permission_groups WHERE id=?`, id))
	if err != nil {
		return Group{}, err
	}
	if group.Version != version {
		return Group{}, auth.ErrConflict
	}
	if group.Name == name {
		return group, nil
	}
	if err := groupNameAvailable(ctx, tx, name, id); err != nil {
		return Group{}, err
	}
	previous := group.Name
	group.Name, group.Version = name, group.Version+1
	if _, err := tx.ExecContext(ctx, `UPDATE permission_groups SET name=?,version=? WHERE id=?`, name, group.Version, id); err != nil {
		return Group{}, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	if err := store.Change(ctx, tx, "system", "group", id, "upsert", group.Version, group, now); err != nil {
		return Group{}, err
	}
	if err := store.Audit(ctx, tx, actor.ID, "group_renamed", "group", id, map[string]string{"previous_name": previous, "name": name}, now); err != nil {
		return Group{}, err
	}
	return group, tx.Commit()
}

func (s *Service) DeleteGroup(ctx context.Context, current auth.Session, id string, version int64) error {
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	group, err := scanGroup(tx.QueryRowContext(ctx, `SELECT id,name,version,created_at FROM permission_groups WHERE id=?`, id))
	if err != nil {
		return err
	}
	if group.Version != version {
		return auth.ErrConflict
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	affected, err := revokeScopes(ctx, tx, `u.id IN (SELECT user_id FROM user_groups WHERE group_id=?)`, []any{id}, now)
	if err != nil {
		return err
	}
	// Keep access_restricted latched while removing RESTRICT-protected edges.
	// Bump the containing resource versions so stale policy edits cannot overwrite
	// the deletion's effects. Shared media and library records are never removed.
	if _, err := tx.ExecContext(ctx, `UPDATE albums SET version=version+1,updated_at=? WHERE id IN (SELECT album_id FROM album_access WHERE group_id=?)`, now.UnixMilli(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE libraries SET version=version+1,updated_at=? WHERE id IN (SELECT library_id FROM library_access WHERE group_id=?)`, now.UnixMilli(), id); err != nil {
		return err
	}
	for _, query := range []string{`DELETE FROM album_access WHERE group_id=?`, `DELETE FROM library_access WHERE group_id=?`, `DELETE FROM permission_groups WHERE id=?`} {
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return err
		}
	}
	if err := store.Change(ctx, tx, "system", "group", id, "delete", version+1, map[string]string{"id": id}, now); err != nil {
		return err
	}
	if err := store.Audit(ctx, tx, actor.ID, "group_deleted", "group", id, map[string]any{"name": group.Name, "affected_users": affected}, now); err != nil {
		return err
	}
	return tx.Commit()
}
