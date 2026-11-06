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

var ErrAlbumRating = errors.New("rating must be one integer from 1 to 5")

// AlbumRating is private to the current account. A null vote retains its version;
// an album never rated by this account starts at version1 with no update instant.
type AlbumRating struct {
	AlbumID   string     `json:"album_id"`
	Rating    *int       `json:"rating"`
	UpdatedAt *time.Time `json:"updated_at"`
	Version   int64      `json:"version"`
}

func albumRatingInTx(ctx context.Context, tx *sql.Tx, viewer, id string) (AlbumRating, int64, error) {
	var value AlbumRating
	var updated sql.NullInt64
	var albumVersion int64
	err := tx.QueryRowContext(ctx, `SELECT a.id,r.rating,r.updated_at,COALESCE(r.version,1),a.version
 FROM albums a JOIN libraries l ON l.id=a.library_id
 LEFT JOIN ratings r ON r.album_id=a.id AND r.user_id=? WHERE `+AlbumVisible+` AND a.id=?`, viewer, viewer, id).
		Scan(&value.AlbumID, &value.Rating, &updated, &value.Version, &albumVersion)
	if err != nil {
		return AlbumRating{}, 0, err
	}
	if updated.Valid {
		instant := time.UnixMilli(updated.Int64).UTC()
		if instant.Year() < 1 || instant.Year() > 9999 {
			return AlbumRating{}, 0, errors.New("stored rating update time is outside the public UTC range")
		}
		value.UpdatedAt = &instant
	}
	return value, albumVersion, nil
}

func (s *Service) AlbumRating(ctx context.Context, current auth.Session, id string) (AlbumRating, error) {
	tx, actor, err := s.readTx(ctx, current)
	if err != nil {
		return AlbumRating{}, err
	}
	defer tx.Rollback()
	value, _, err := albumRatingInTx(ctx, tx, actor.ID, id)
	return value, err
}

// SetAlbumRating replaces only this actor's vote; nil explicitly clears it.
// Fresh visibility precedes receipt replay, including after a grant was revoked.
func (s *Service) SetAlbumRating(ctx context.Context, current auth.Session, id string, version int64, rating *int, key string) (AlbumRating, error) {
	if rating != nil && (*rating < 1 || *rating > 5) {
		return AlbumRating{}, ErrAlbumRating
	}
	tx, actor, err := s.readTx(ctx, current)
	if err != nil {
		return AlbumRating{}, err
	}
	defer tx.Rollback()
	before, albumVersion, err := albumRatingInTx(ctx, tx, actor.ID, id)
	if err != nil {
		return AlbumRating{}, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
		Rating  *int   `json:"rating"`
	}{id, version, rating})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "album.rating.replace", Metadata: string(data)}
	var result AlbumRating
	if found, err := command.Replay(ctx, tx, now, &result); found || err != nil {
		return result, err
	}
	if before.Version != version {
		return AlbumRating{}, auth.ErrConflict
	}
	changed := (before.Rating == nil) != (rating == nil) || before.Rating != nil && rating != nil && *before.Rating != *rating
	if changed {
		if version == math.MaxInt64 || albumVersion == math.MaxInt64 {
			return AlbumRating{}, auth.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ratings(user_id,album_id,rating,updated_at,version) VALUES(?,?,?,?,?)
 ON CONFLICT(user_id,album_id) DO UPDATE SET rating=excluded.rating,updated_at=max(ratings.updated_at,excluded.updated_at),version=excluded.version`, actor.ID, id, rating, now.UnixMilli(), version+1); err != nil {
			return AlbumRating{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE albums SET version=version+1,updated_at=max(updated_at,?) WHERE id=?`, now.UnixMilli(), id); err != nil {
			return AlbumRating{}, err
		}
	}
	result, _, err = albumRatingInTx(ctx, tx, actor.ID, id)
	if err != nil {
		return AlbumRating{}, err
	}
	if changed {
		if err := store.JournalAlbumTx(ctx, tx, id, now); err != nil {
			return AlbumRating{}, err
		}
		op := "upsert"
		if rating == nil {
			op = "delete"
		}
		if err := store.Change(ctx, tx, actor.ScopeID, "album_rating", id, op, result.Version, result, now); err != nil {
			return AlbumRating{}, err
		}
		if err := store.Audit(ctx, tx, actor.ID, "album_rating_changed", "album", id, map[string]any{"previous_rating": before.Rating, "rating": result.Rating, "version": result.Version}, now); err != nil {
			return AlbumRating{}, err
		}
	}
	if err := command.Save(ctx, tx, result, now); err != nil {
		return AlbumRating{}, err
	}
	return result, tx.Commit()
}
