package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

var (
	ErrTagSet     = errors.New("provide an explicit array of at most 100 unique tag IDs")
	ErrUnknownTag = errors.New("one or more selected tags do not exist")
)

// AlbumTagSet contains only association IDs and the shared album version. Tag
// label changes do not invalidate this body; label-bearing public pages have no ETag.
type AlbumTagSet struct {
	AlbumID string   `json:"album_id"`
	TagIDs  []string `json:"tag_ids"`
	Version int64    `json:"version"`
}

func albumTagIDsInTx(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT tag_id FROM album_tags WHERE album_id=? ORDER BY tag_id LIMIT 101`, id)
	if err != nil {
		return nil, err
	}
	// Tags share the existing bounded opaque-ID set grammar, not group authority.
	ids, err := scanGroupIDs(rows)
	if errors.Is(err, ErrGroupSet) {
		return nil, ErrTagSet
	}
	return ids, err
}

func albumTagSetInTx(ctx context.Context, tx *sql.Tx, viewer, id string) (AlbumTagSet, error) {
	var value AlbumTagSet
	if err := tx.QueryRowContext(ctx, `SELECT a.id,a.version FROM albums a JOIN libraries l ON l.id=a.library_id WHERE `+AlbumVisible+` AND a.id=?`, viewer, id).Scan(&value.AlbumID, &value.Version); err != nil {
		return AlbumTagSet{}, err
	}
	var err error
	value.TagIDs, err = albumTagIDsInTx(ctx, tx, id)
	return value, err
}

func (s *Service) AdminAlbumTags(ctx context.Context, current auth.Session, id string) (AlbumTagSet, error) {
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return AlbumTagSet{}, err
	}
	defer tx.Rollback()
	return albumTagSetInTx(ctx, tx, actor.ID, id)
}

func (s *Service) AlbumTags(ctx context.Context, current auth.Session, id, after string, limit int) ([]Tag, error) {
	if !pageLimit(limit) {
		return nil, ErrTagSet
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var found string
	if err := tx.QueryRowContext(ctx, `SELECT a.id FROM albums a JOIN libraries l ON l.id=a.library_id WHERE `+AlbumVisible+` AND a.id=?`, viewer.ID, id).Scan(&found); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.id,t.name,t.version FROM tags t JOIN album_tags edge ON edge.tag_id=t.id
 JOIN albums a ON a.id=edge.album_id JOIN libraries l ON l.id=a.library_id
 WHERE `+AlbumVisible+` AND a.id=? AND t.id>? ORDER BY t.id LIMIT ?`, viewer.ID, id, after, limit)
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

func (s *Service) SetAlbumTags(ctx context.Context, current auth.Session, id string, version int64, ids []string, key string) (AlbumTagSet, error) {
	ids, data, err := normalizedGroups(ids)
	if err != nil {
		return AlbumTagSet{}, ErrTagSet
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return AlbumTagSet{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Millisecond)
	request, _ := json.Marshal(struct {
		ID      string   `json:"id"`
		Version int64    `json:"version"`
		TagIDs  []string `json:"tag_ids"`
	}{id, version, ids})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "album.tags.replace", Metadata: string(request)}
	var result AlbumTagSet
	if found, err := command.Replay(ctx, tx, now, &result); found || err != nil {
		return result, err
	}
	result, err = albumTagSetInTx(ctx, tx, actor.ID, id)
	if err != nil {
		return AlbumTagSet{}, err
	}
	if result.Version != version {
		return AlbumTagSet{}, auth.ErrConflict
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags WHERE id IN (SELECT value FROM json_each(?))`, data).Scan(&count); err != nil {
		return AlbumTagSet{}, err
	}
	if count != len(ids) {
		return AlbumTagSet{}, ErrUnknownTag
	}
	if !slices.Equal(result.TagIDs, ids) {
		if version == math.MaxInt64 {
			return AlbumTagSet{}, auth.ErrConflict
		}
		previous := result.TagIDs
		if _, err := tx.ExecContext(ctx, `DELETE FROM album_tags WHERE album_id=?`, id); err != nil {
			return AlbumTagSet{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO album_tags(album_id,tag_id) SELECT ?,value FROM json_each(?)`, id, data); err != nil {
			return AlbumTagSet{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE albums SET version=version+1,updated_at=max(updated_at,?) WHERE id=?`, now.UnixMilli(), id); err != nil {
			return AlbumTagSet{}, err
		}
		result.TagIDs, result.Version = ids, version+1
		if err := JournalAlbumTx(ctx, tx, id, now); err != nil {
			return AlbumTagSet{}, err
		}
		if err := store.Audit(ctx, tx, actor.ID, "album_tags_changed", "album", id, map[string]any{"previous_tag_ids": previous, "tag_ids": ids, "version": result.Version}, now); err != nil {
			return AlbumTagSet{}, err
		}
	}
	if err := command.Save(ctx, tx, result, now); err != nil {
		return AlbumTagSet{}, err
	}
	return result, tx.Commit()
}
