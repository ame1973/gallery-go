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

// AlbumView is private to the current account; the shared total is on Album.
// An album never viewed by this account starts at version1 without a timestamp.
// Counts are server-recorded accepted events, not media-download completions.
type AlbumView struct {
	AlbumID      string     `json:"album_id"`
	ViewCount    int64      `json:"view_count"`
	LastViewedAt *time.Time `json:"last_viewed_at"`
	Version      int64      `json:"version"`
}

func albumViewInTx(ctx context.Context, tx *sql.Tx, viewer, id string) (AlbumView, int64, int64, error) {
	var value AlbumView
	var last sql.NullInt64
	var albumVersion, total int64
	err := tx.QueryRowContext(ctx, `SELECT a.id,COALESCE(v.view_count,0),v.last_viewed_at,COALESCE(v.version,1),a.version,a.view_count
 FROM albums a JOIN libraries l ON l.id=a.library_id
 LEFT JOIN album_views v ON v.album_id=a.id AND v.user_id=? WHERE `+AlbumVisible+` AND a.id=?`, viewer, viewer, id).
		Scan(&value.AlbumID, &value.ViewCount, &last, &value.Version, &albumVersion, &total)
	if err != nil {
		return AlbumView{}, 0, 0, err
	}
	if last.Valid {
		instant := time.UnixMilli(last.Int64).UTC()
		if instant.Year() < 1 || instant.Year() > 9999 {
			return AlbumView{}, 0, 0, errors.New("stored album view time is outside the public UTC range")
		}
		value.LastViewedAt = &instant
	}
	return value, albumVersion, total, nil
}

func (s *Service) AlbumView(ctx context.Context, current auth.Session, id string) (AlbumView, error) {
	tx, actor, err := s.readTx(ctx, current)
	if err != nil {
		return AlbumView{}, err
	}
	defer tx.Rollback()
	value, _, _, err := albumViewInTx(ctx, tx, actor.ID, id)
	return value, err
}

// RecordAlbumView records exactly one accepted online event. A matching actor/key
// retry returns the historical personal result without incrementing again. Fresh
// visibility precedes receipt replay; source bytes and download grants are unchanged.
func (s *Service) RecordAlbumView(ctx context.Context, current auth.Session, id, key string) (AlbumView, error) {
	tx, actor, err := s.readTx(ctx, current)
	if err != nil {
		return AlbumView{}, err
	}
	defer tx.Rollback()
	before, albumVersion, total, err := albumViewInTx(ctx, tx, actor.ID, id)
	if err != nil {
		return AlbumView{}, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	if now.Year() < 1 || now.Year() > 9999 {
		return AlbumView{}, errors.New("server clock is outside the public UTC range")
	}
	data, _ := json.Marshal(struct {
		ID string `json:"id"`
	}{id})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "album.view.record", Metadata: string(data)}
	var result AlbumView
	if found, err := command.Replay(ctx, tx, now, &result); found || err != nil {
		return result, err
	}
	if before.ViewCount == math.MaxInt64 || before.Version == math.MaxInt64 || albumVersion == math.MaxInt64 || total == math.MaxInt64 {
		return AlbumView{}, auth.ErrConflict
	}
	if before.ViewCount == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO album_views(user_id,album_id,view_count,last_viewed_at,version) VALUES(?,?,1,?,2)`, actor.ID, id, now.UnixMilli())
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE album_views SET view_count=view_count+1,last_viewed_at=max(last_viewed_at,?),version=version+1 WHERE user_id=? AND album_id=?`, now.UnixMilli(), actor.ID, id)
	}
	if err != nil {
		return AlbumView{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE albums SET version=version+1,updated_at=max(updated_at,?) WHERE id=?`, now.UnixMilli(), id); err != nil {
		return AlbumView{}, err
	}
	result, _, _, err = albumViewInTx(ctx, tx, actor.ID, id)
	if err != nil {
		return AlbumView{}, err
	}
	if err := store.JournalAlbumTx(ctx, tx, id, now); err != nil {
		return AlbumView{}, err
	}
	if err := store.Change(ctx, tx, actor.ScopeID, "album_view", id, "upsert", result.Version, result, now); err != nil {
		return AlbumView{}, err
	}
	if err := command.Save(ctx, tx, result, now); err != nil {
		return AlbumView{}, err
	}
	return result, tx.Commit()
}
