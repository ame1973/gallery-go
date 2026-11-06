package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

var (
	ErrTagNameTaken = errors.New("the exact tag label is already in use")
	ErrTagInUse     = errors.New("remove the tag's album associations explicitly before deleting it")
)

// Catalog labels retain SQLite's exact binary identity after display-label
// validation. This is not multilingual search normalization or a content grant.
type Tag struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version int64  `json:"version"`
}

func scanTag(row interface{ Scan(...any) error }) (Tag, error) {
	var tag Tag
	err := row.Scan(&tag.ID, &tag.Name, &tag.Version)
	return tag, err
}

func (s *Service) Tags(ctx context.Context, current auth.Session, after string, limit int) ([]Tag, error) {
	if !pageLimit(limit) {
		return nil, ErrGroupSet
	}
	tx, _, err := s.adminTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,name,version FROM tags WHERE id>? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tags := []Tag{}
	for rows.Next() {
		tag, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

func (s *Service) Tag(ctx context.Context, current auth.Session, id string) (Tag, error) {
	tx, _, err := s.adminTx(ctx, current)
	if err != nil {
		return Tag{}, err
	}
	defer tx.Rollback()
	return scanTag(tx.QueryRowContext(ctx, `SELECT id,name,version FROM tags WHERE id=?`, id))
}

func tagNameAvailable(ctx context.Context, tx *sql.Tx, name, exceptID string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tags WHERE name=? AND id!=?)`, name, exceptID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrTagNameTaken
	}
	return nil
}

func (s *Service) CreateTag(ctx context.Context, current auth.Session, name, key string) (Tag, error) {
	name, err := normalizedName(name)
	if err != nil {
		return Tag{}, err
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return Tag{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(map[string]string{"name": name})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "tag.create", Metadata: string(data)}
	var tag Tag
	if found, err := command.Replay(ctx, tx, now, &tag); found || err != nil {
		return tag, err
	}
	if err := tagNameAvailable(ctx, tx, name, ""); err != nil {
		return Tag{}, err
	}
	tag = Tag{ID: store.NewID(), Name: name, Version: 1}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tags(id,name,version) VALUES(?,?,1)`, tag.ID, tag.Name); err != nil {
		return Tag{}, err
	}
	if err := store.Change(ctx, tx, "system", "tag", tag.ID, "upsert", 1, tag, now); err != nil {
		return Tag{}, err
	}
	if err := store.Audit(ctx, tx, actor.ID, "tag_created", "tag", tag.ID, map[string]string{"name": name}, now); err != nil {
		return Tag{}, err
	}
	if err := command.Save(ctx, tx, tag, now); err != nil {
		return Tag{}, err
	}
	return tag, tx.Commit()
}

func (s *Service) RenameTag(ctx context.Context, current auth.Session, id string, version int64, name, key string) (Tag, error) {
	name, err := normalizedName(name)
	if err != nil {
		return Tag{}, err
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return Tag{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
		Name    string `json:"name"`
	}{id, version, name})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "tag.rename", Metadata: string(data)}
	var tag Tag
	if found, err := command.Replay(ctx, tx, now, &tag); found || err != nil {
		return tag, err
	}
	tag, err = scanTag(tx.QueryRowContext(ctx, `SELECT id,name,version FROM tags WHERE id=?`, id))
	if err != nil {
		return Tag{}, err
	}
	if tag.Version != version {
		return Tag{}, auth.ErrConflict
	}
	if tag.Name != name {
		if version == math.MaxInt64 {
			return Tag{}, auth.ErrConflict
		}
		if err := tagNameAvailable(ctx, tx, name, id); err != nil {
			return Tag{}, err
		}
		tag.Name, tag.Version = name, tag.Version+1
		if _, err := tx.ExecContext(ctx, `UPDATE tags SET name=?,version=? WHERE id=?`, tag.Name, tag.Version, id); err != nil {
			return Tag{}, err
		}
		if err := store.Change(ctx, tx, "system", "tag", id, "upsert", tag.Version, tag, now); err != nil {
			return Tag{}, err
		}
		if err := store.Audit(ctx, tx, actor.ID, "tag_renamed", "tag", id, map[string]any{"name": name, "version": tag.Version}, now); err != nil {
			return Tag{}, err
		}
	}
	if err := command.Save(ctx, tx, tag, now); err != nil {
		return Tag{}, err
	}
	return tag, tx.Commit()
}

func (s *Service) DeleteTag(ctx context.Context, current auth.Session, id string, version int64, key string) error {
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}{id, version})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "tag.delete", Metadata: string(data)}
	var result struct{}
	if found, err := command.Replay(ctx, tx, now, &result); found || err != nil {
		return err
	}
	tag, err := scanTag(tx.QueryRowContext(ctx, `SELECT id,name,version FROM tags WHERE id=?`, id))
	if err != nil {
		return err
	}
	if tag.Version != version || version == math.MaxInt64 {
		return auth.ErrConflict
	}
	var used bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM album_tags WHERE tag_id=?)`, id).Scan(&used); err != nil {
		return err
	}
	if used {
		return ErrTagInUse
	}
	// The existing schema has cascade edges, so an explicit in-transaction check
	// must reject every association, including inactive or hidden albums.
	if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE id=?`, id); err != nil {
		return err
	}
	if err := store.Change(ctx, tx, "system", "tag", id, "delete", version+1, map[string]string{"id": id}, now); err != nil {
		return err
	}
	if err := store.Audit(ctx, tx, actor.ID, "tag_deleted", "tag", id, map[string]any{"name": tag.Name, "version": version + 1}, now); err != nil {
		return err
	}
	if err := command.Save(ctx, tx, result, now); err != nil {
		return err
	}
	return tx.Commit()
}
