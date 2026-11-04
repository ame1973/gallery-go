package access

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

var ErrLibraryNotEmpty = errors.New("a library with sources or albums cannot be deleted; deactivate it instead")

type Library struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Active    bool      `json:"active"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

type LibraryPatch struct {
	Name   *string `json:"name"`
	Active *bool   `json:"active"`
}

const librarySelect = `SELECT l.id,l.name,l.is_active,l.version,l.created_at FROM libraries l`

func scanLibrary(row interface{ Scan(...any) error }) (Library, error) {
	var library Library
	var created int64
	err := row.Scan(&library.ID, &library.Name, &library.Active, &library.Version, &created)
	library.CreatedAt = time.UnixMilli(created).UTC()
	return library, err
}

func (s *Service) AdminLibraries(ctx context.Context, current auth.Session, after string, limit int) ([]Library, error) {
	if !pageLimit(limit) {
		return nil, ErrGroupSet
	}
	tx, _, err := s.adminTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, librarySelect+` WHERE l.id>? ORDER BY l.id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Library, 0)
	for rows.Next() {
		library, err := scanLibrary(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, library)
	}
	return items, rows.Err()
}

func (s *Service) AdminLibrary(ctx context.Context, current auth.Session, id string) (Library, error) {
	tx, _, err := s.adminTx(ctx, current)
	if err != nil {
		return Library{}, err
	}
	defer tx.Rollback()
	return scanLibrary(tx.QueryRowContext(ctx, librarySelect+` WHERE l.id=?`, id))
}

func (s *Service) CreateLibrary(ctx context.Context, current auth.Session, name string, active bool, key string) (Library, error) {
	name, err := normalizedName(name)
	if err != nil {
		return Library{}, err
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return Library{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(struct {
		Name   string `json:"name"`
		Active bool   `json:"active"`
	}{name, active})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "library.create", Metadata: string(data)}
	var library Library
	if found, err := command.Replay(ctx, tx, now, &library); found || err != nil {
		return library, err
	}
	library = Library{ID: store.NewID(), Name: name, Active: active, Version: 1, CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO libraries(id,name,is_active,version,created_at,updated_at) VALUES(?,?,?,1,?,?)`, library.ID, name, active, now.UnixMilli(), now.UnixMilli()); err != nil {
		return Library{}, err
	}
	if err := store.Change(ctx, tx, "system", "library", library.ID, "upsert", 1, library, now); err != nil {
		return Library{}, err
	}
	if err := store.Audit(ctx, tx, actor.ID, "library_created", "library", library.ID, map[string]any{"name": name, "active": active}, now); err != nil {
		return Library{}, err
	}
	if err := command.Save(ctx, tx, library, now); err != nil {
		return Library{}, err
	}
	return library, tx.Commit()
}

func (s *Service) UpdateLibrary(ctx context.Context, current auth.Session, id string, version int64, patch LibraryPatch) (Library, error) {
	if patch.Name == nil && patch.Active == nil {
		return Library{}, ErrName
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return Library{}, err
	}
	defer tx.Rollback()
	library, err := scanLibrary(tx.QueryRowContext(ctx, librarySelect+` WHERE l.id=?`, id))
	if err != nil {
		return Library{}, err
	}
	if library.Version != version {
		return Library{}, auth.ErrConflict
	}
	before := library
	if patch.Name != nil {
		library.Name, err = normalizedName(*patch.Name)
		if err != nil {
			return Library{}, err
		}
	}
	if patch.Active != nil {
		library.Active = *patch.Active
	}
	if before == library {
		return library, nil
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	if library.Active != before.Active {
		if _, err := revokeScopes(ctx, tx, `u.role='admin' OR u.id IN (
		 SELECT ug.user_id FROM user_groups ug JOIN library_access la ON la.group_id=ug.group_id WHERE la.library_id=?)`, []any{id}, now); err != nil {
			return Library{}, err
		}
	}
	library.Version++
	if _, err := tx.ExecContext(ctx, `UPDATE libraries SET name=?,is_active=?,version=?,updated_at=? WHERE id=?`, library.Name, library.Active, library.Version, now.UnixMilli(), id); err != nil {
		return Library{}, err
	}
	if err := store.Change(ctx, tx, "system", "library", id, "upsert", library.Version, library, now); err != nil {
		return Library{}, err
	}
	if err := store.Audit(ctx, tx, actor.ID, "library_updated", "library", id, map[string]any{"name": library.Name, "active": library.Active, "version": library.Version}, now); err != nil {
		return Library{}, err
	}
	return library, tx.Commit()
}

func (s *Service) DeleteLibrary(ctx context.Context, current auth.Session, id string, version int64) error {
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	library, err := scanLibrary(tx.QueryRowContext(ctx, librarySelect+` WHERE l.id=?`, id))
	if err != nil {
		return err
	}
	if library.Version != version {
		return auth.ErrConflict
	}
	var occupied bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sources WHERE library_id=?) OR EXISTS(SELECT 1 FROM albums WHERE library_id=?)`, id, id).Scan(&occupied); err != nil {
		return err
	}
	if occupied {
		return ErrLibraryNotEmpty
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	if _, err := revokeScopes(ctx, tx, `u.role='admin' OR u.id IN (
	 SELECT ug.user_id FROM user_groups ug JOIN library_access la ON la.group_id=ug.group_id WHERE la.library_id=?)`, []any{id}, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM libraries WHERE id=?`, id); err != nil {
		return err
	}
	if err := store.Change(ctx, tx, "system", "library", id, "delete", version+1, map[string]string{"id": id}, now); err != nil {
		return err
	}
	if err := store.Audit(ctx, tx, actor.ID, "library_deleted", "library", id, map[string]string{"name": library.Name}, now); err != nil {
		return err
	}
	return tx.Commit()
}
