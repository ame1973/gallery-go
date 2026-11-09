package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

var ErrAlbumFavorite = errors.New("collection_id must be null or a nonempty literal ID of at most 64 UTF-8 bytes; cleared favorites cannot name a collection")

// AlbumFavorite is private to this account. A cleared membership keeps its version;
// never-favorited albums start at version1 without a persisted update instant.
type AlbumFavorite struct {
	AlbumID      string     `json:"album_id"`
	Favorited    bool       `json:"favorited"`
	CollectionID *string    `json:"collection_id"`
	UpdatedAt    *time.Time `json:"updated_at"`
	Version      int64      `json:"version"`
}

func albumFavoriteInTx(ctx context.Context, tx *sql.Tx, viewer, id string) (AlbumFavorite, error) {
	var value AlbumFavorite
	var updated sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT a.id,COALESCE(f.favorited,0),f.collection_id,f.updated_at,COALESCE(f.version,1)
 FROM albums a JOIN libraries l ON l.id=a.library_id
 LEFT JOIN favorites f ON f.album_id=a.id AND f.user_id=? WHERE `+AlbumVisible+` AND a.id=?`, viewer, viewer, id).
		Scan(&value.AlbumID, &value.Favorited, &value.CollectionID, &updated, &value.Version)
	if err != nil {
		return AlbumFavorite{}, err
	}
	if updated.Valid {
		instant := time.UnixMilli(updated.Int64).UTC()
		if instant.Year() < 1 || instant.Year() > 9999 {
			return AlbumFavorite{}, errors.New("stored favorite update time is outside the public UTC range")
		}
		value.UpdatedAt = &instant
	}
	return value, nil
}

func (s *Service) AlbumFavorite(ctx context.Context, current auth.Session, id string) (AlbumFavorite, error) {
	tx, actor, err := s.readTx(ctx, current)
	if err != nil {
		return AlbumFavorite{}, err
	}
	defer tx.Rollback()
	return albumFavoriteInTx(ctx, tx, actor.ID, id)
}

// SetAlbumFavorite changes only the current actor's membership/assignment. It does
// not edit shared gallery versions, grant content access or replay without visibility.
func (s *Service) SetAlbumFavorite(ctx context.Context, current auth.Session, id string, version int64, favorited bool, collection *string, key string) (AlbumFavorite, error) {
	if collection != nil {
		if !favorited || *collection == "" || len(*collection) > 64 || !utf8.ValidString(*collection) || strings.TrimSpace(*collection) != *collection || strings.ContainsAny(*collection, "/\\") {
			return AlbumFavorite{}, ErrAlbumFavorite
		}
		for _, character := range *collection {
			if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
				return AlbumFavorite{}, ErrAlbumFavorite
			}
		}
	}
	tx, actor, err := s.readTx(ctx, current)
	if err != nil {
		return AlbumFavorite{}, err
	}
	defer tx.Rollback()
	before, err := albumFavoriteInTx(ctx, tx, actor.ID, id)
	if err != nil {
		return AlbumFavorite{}, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(struct {
		ID           string  `json:"id"`
		Version      int64   `json:"version"`
		Favorited    bool    `json:"favorited"`
		CollectionID *string `json:"collection_id"`
	}{id, version, favorited, collection})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "album.favorite.replace", Metadata: string(data)}
	var result AlbumFavorite
	if found, err := command.Replay(ctx, tx, now, &result); found || err != nil {
		return result, err
	}
	if before.Version != version {
		return AlbumFavorite{}, auth.ErrConflict
	}
	if collection != nil {
		var owned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM favorite_collections WHERE user_id=? AND id=?)`, actor.ID, *collection).Scan(&owned); err != nil {
			return AlbumFavorite{}, err
		}
		if !owned {
			return AlbumFavorite{}, sql.ErrNoRows
		}
	}
	changed := before.Favorited != favorited || (before.CollectionID == nil) != (collection == nil) || before.CollectionID != nil && collection != nil && *before.CollectionID != *collection
	if changed {
		if version == math.MaxInt64 {
			return AlbumFavorite{}, auth.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO favorites(user_id,album_id,collection_id,created_at,favorited,updated_at,version) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(user_id,album_id) DO UPDATE SET collection_id=excluded.collection_id,favorited=excluded.favorited,updated_at=max(favorites.updated_at,excluded.updated_at),version=excluded.version`, actor.ID, id, collection, now.UnixMilli(), favorited, now.UnixMilli(), version+1); err != nil {
			return AlbumFavorite{}, err
		}
	}
	result, err = albumFavoriteInTx(ctx, tx, actor.ID, id)
	if err != nil {
		return AlbumFavorite{}, err
	}
	if changed {
		op := "upsert"
		if !favorited {
			op = "delete"
		}
		if err := store.Change(ctx, tx, actor.ScopeID, "favorite", id, op, result.Version, result, now); err != nil {
			return AlbumFavorite{}, err
		}
		if err := store.Audit(ctx, tx, actor.ID, "album_favorite_changed", "album", id, map[string]any{"previous_favorited": before.Favorited, "previous_collection_id": before.CollectionID, "favorited": result.Favorited, "collection_id": result.CollectionID, "version": result.Version}, now); err != nil {
			return AlbumFavorite{}, err
		}
	}
	if err := command.Save(ctx, tx, result, now); err != nil {
		return AlbumFavorite{}, err
	}
	return result, tx.Commit()
}
