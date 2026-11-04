package access

import (
	"context"
	"database/sql"
	"slices"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

type LibraryPolicy struct {
	LibraryID string   `json:"library_id"`
	GroupIDs  []string `json:"group_ids"`
	Version   int64    `json:"version"`
}

type AlbumPolicy struct {
	AlbumID    string   `json:"album_id"`
	Restricted bool     `json:"restricted"`
	GroupIDs   []string `json:"group_ids"`
	Version    int64    `json:"version"`
}

func libraryPolicyInTx(ctx context.Context, tx *sql.Tx, id string) (LibraryPolicy, error) {
	policy := LibraryPolicy{LibraryID: id}
	if err := tx.QueryRowContext(ctx, `SELECT version FROM libraries WHERE id=?`, id).Scan(&policy.Version); err != nil {
		return LibraryPolicy{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT group_id FROM library_access WHERE library_id=? ORDER BY group_id LIMIT 101`, id)
	if err != nil {
		return LibraryPolicy{}, err
	}
	policy.GroupIDs, err = scanGroupIDs(rows)
	return policy, err
}

func albumPolicyInTx(ctx context.Context, tx *sql.Tx, id string) (AlbumPolicy, error) {
	policy := AlbumPolicy{AlbumID: id}
	if err := tx.QueryRowContext(ctx, `SELECT access_restricted,version FROM albums WHERE id=?`, id).Scan(&policy.Restricted, &policy.Version); err != nil {
		return AlbumPolicy{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT group_id FROM album_access WHERE album_id=? ORDER BY group_id LIMIT 101`, id)
	if err != nil {
		return AlbumPolicy{}, err
	}
	policy.GroupIDs, err = scanGroupIDs(rows)
	return policy, err
}

func (s *Service) LibraryPolicy(ctx context.Context, current auth.Session, id string) (LibraryPolicy, error) {
	tx, _, err := s.adminTx(ctx, current)
	if err != nil {
		return LibraryPolicy{}, err
	}
	defer tx.Rollback()
	return libraryPolicyInTx(ctx, tx, id)
}

func (s *Service) AlbumPolicy(ctx context.Context, current auth.Session, id string) (AlbumPolicy, error) {
	tx, _, err := s.adminTx(ctx, current)
	if err != nil {
		return AlbumPolicy{}, err
	}
	defer tx.Rollback()
	return albumPolicyInTx(ctx, tx, id)
}

func (s *Service) SetLibraryPolicy(ctx context.Context, current auth.Session, id string, version int64, ids []string) (LibraryPolicy, error) {
	ids, data, err := normalizedGroups(ids)
	if err != nil {
		return LibraryPolicy{}, err
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return LibraryPolicy{}, err
	}
	defer tx.Rollback()
	before, err := libraryPolicyInTx(ctx, tx, id)
	if err != nil {
		return LibraryPolicy{}, err
	}
	if before.Version != version {
		return LibraryPolicy{}, auth.ErrConflict
	}
	if err := knownGroups(ctx, tx, data, len(ids)); err != nil {
		return LibraryPolicy{}, err
	}
	if slices.Equal(before.GroupIDs, ids) {
		return before, nil
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	affected, err := revokeScopes(ctx, tx, `u.id IN (
	 SELECT ug.user_id FROM user_groups ug WHERE ug.group_id IN (
	 SELECT group_id FROM library_access WHERE library_id=? UNION SELECT value FROM json_each(?)))`, []any{id, data}, now)
	if err != nil {
		return LibraryPolicy{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM library_access WHERE library_id=?`, id); err != nil {
		return LibraryPolicy{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO library_access(library_id,group_id) SELECT ?,value FROM json_each(?)`, id, data); err != nil {
		return LibraryPolicy{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE libraries SET version=version+1,updated_at=? WHERE id=?`, now.UnixMilli(), id); err != nil {
		return LibraryPolicy{}, err
	}
	policy := LibraryPolicy{LibraryID: id, GroupIDs: ids, Version: version + 1}
	if err := store.Change(ctx, tx, "system", "library_access", id, "upsert", policy.Version, policy, now); err != nil {
		return LibraryPolicy{}, err
	}
	if err := store.Audit(ctx, tx, actor.ID, "library_access_changed", "library", id, map[string]any{"previous_group_ids": before.GroupIDs, "group_ids": ids, "affected_users": affected, "version": policy.Version}, now); err != nil {
		return LibraryPolicy{}, err
	}
	return policy, tx.Commit()
}

func (s *Service) SetAlbumPolicy(ctx context.Context, current auth.Session, id string, version int64, restricted bool, ids []string) (AlbumPolicy, error) {
	ids, data, err := normalizedGroups(ids)
	if err != nil {
		return AlbumPolicy{}, err
	}
	if !restricted && len(ids) != 0 {
		return AlbumPolicy{}, ErrRestriction
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return AlbumPolicy{}, err
	}
	defer tx.Rollback()
	before, err := albumPolicyInTx(ctx, tx, id)
	if err != nil {
		return AlbumPolicy{}, err
	}
	if before.Version != version {
		return AlbumPolicy{}, auth.ErrConflict
	}
	if err := knownGroups(ctx, tx, data, len(ids)); err != nil {
		return AlbumPolicy{}, err
	}
	if before.Restricted == restricted && slices.Equal(before.GroupIDs, ids) {
		return before, nil
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	affected, err := revokeScopes(ctx, tx, `u.id IN (
	 SELECT ug.user_id FROM user_groups ug JOIN library_access la ON la.group_id=ug.group_id
	 JOIN albums a ON a.library_id=la.library_id WHERE a.id=?)`, []any{id}, now)
	if err != nil {
		return AlbumPolicy{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM album_access WHERE album_id=?`, id); err != nil {
		return AlbumPolicy{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO album_access(album_id,group_id) SELECT ?,value FROM json_each(?)`, id, data); err != nil {
		return AlbumPolicy{}, err
	}
	// Only this explicit policy write can clear the restriction latch. Group or
	// grant deletion never infers "unrestricted" from an empty relation table.
	if _, err := tx.ExecContext(ctx, `UPDATE albums SET access_restricted=?,version=version+1,updated_at=? WHERE id=?`, restricted, now.UnixMilli(), id); err != nil {
		return AlbumPolicy{}, err
	}
	policy := AlbumPolicy{AlbumID: id, Restricted: restricted, GroupIDs: ids, Version: version + 1}
	if err := store.Change(ctx, tx, "system", "album_access", id, "upsert", policy.Version, policy, now); err != nil {
		return AlbumPolicy{}, err
	}
	if err := store.Audit(ctx, tx, actor.ID, "album_access_changed", "album", id, map[string]any{"previous_restricted": before.Restricted, "previous_group_ids": before.GroupIDs, "restricted": restricted, "group_ids": ids, "affected_users": affected, "version": policy.Version}, now); err != nil {
		return AlbumPolicy{}, err
	}
	return policy, tx.Commit()
}
