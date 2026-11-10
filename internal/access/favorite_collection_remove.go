package access

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

const favoriteDeleteFolderStageSQL = favoriteDeleteSubtreeSQL + `
INSERT INTO favorite_delete_folders(id,version)
 SELECT c.id,c.version FROM subtree s CROSS JOIN favorite_collections c INDEXED BY uq_collection_owner_id
 WHERE c.user_id=? AND c.id=s.id`

const favoriteDeleteItemStageSQL = `INSERT INTO favorite_delete_items(album_id,version,updated_at)
 SELECT f.album_id,f.version,f.updated_at FROM favorite_delete_folders d
 CROSS JOIN favorites f INDEXED BY idx_favorites_collection
 WHERE f.user_id=? AND f.collection_id=d.id`

// DeleteFavoriteCollection acts on the exact confirmed private subtree, including
// memberships hidden by later grant changes. It never exposes their identifiers.
// All rows, personal events, the deletion audit and receipt commit together.
func (s *Service) DeleteFavoriteCollection(ctx context.Context, current auth.Session, id string, version int64, action, token, key string) error {
	if !validFavoriteDeleteAction(action) || len(token) != sha256.Size*2 || token != strings.ToLower(token) {
		return ErrFavoriteCollectionDelete
	}
	confirmation, err := hex.DecodeString(token)
	if err != nil {
		return ErrFavoriteCollectionDelete
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actor, err := s.auth.SessionInTx(ctx, tx, current)
	if err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
		Action  string `json:"action"`
		Token   string `json:"confirmation_token"`
	}{id, version, action, token})
	command := store.Command{Actor: actor.User.ID, Key: key, Operation: "favorite_collection.delete", Metadata: string(data)}
	var result struct{}
	if found, err := command.Replay(ctx, tx, now, &result); found || err != nil {
		return err
	}
	folder, err := ownedFavoriteCollection(ctx, tx, actor.User.ID, id)
	if err != nil {
		return err
	}
	if folder.Version != version {
		return auth.ErrConflict
	}
	if err := checkFavoriteDeleteAncestry(ctx, tx, actor.User.ID, folder); err != nil {
		return err
	}
	secret, err := favoriteDeleteSecret(ctx, tx)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("favorite deletion confirmation key is unavailable")
	}
	if err != nil {
		return err
	}
	expected, err := favoriteCollectionDeleteMAC(ctx, tx, secret, actor, id, action)
	if err != nil {
		return err
	}
	if !hmac.Equal(confirmation, expected) {
		return auth.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE favorite_delete_folders(id TEXT PRIMARY KEY,version INTEGER NOT NULL) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE favorite_delete_items(album_id TEXT PRIMARY KEY,version INTEGER NOT NULL,updated_at INTEGER NOT NULL) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, favoriteDeleteFolderStageSQL, actor.User.ID, id, actor.User.ID, actor.User.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, favoriteDeleteItemStageSQL, actor.User.ID); err != nil {
		return err
	}
	var folderCount int64
	var invalid bool
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM favorite_delete_folders),
 EXISTS(SELECT 1 FROM favorite_delete_folders WHERE version>=? OR version<1)
 OR EXISTS(SELECT 1 FROM favorite_delete_items WHERE version>=? OR version<1)`, int64(math.MaxInt64), int64(math.MaxInt64)).Scan(&folderCount, &invalid); err != nil {
		return err
	}
	if folderCount == 0 {
		return sql.ErrNoRows
	}
	if invalid {
		return auth.ErrConflict
	}
	if err := applyFavoriteCollectionDeletion(ctx, tx, actor.User, action, now); err != nil {
		return err
	}
	if err := journalDeletedFavoriteCollections(ctx, tx, actor.User, now); err != nil {
		return err
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM favorite_collections WHERE user_id=? AND id IN (SELECT id FROM favorite_delete_folders)`, actor.User.ID)
	if err != nil {
		return err
	}
	count, err := deleted.RowsAffected()
	if err != nil {
		return err
	}
	if count != folderCount {
		return errors.New("confirmed favorite collection subtree changed during deletion")
	}
	if err := store.Audit(ctx, tx, actor.User.ID, "favorite_collection_deleted", "favorite_collection", id, map[string]any{"action": action, "version": version + 1}, now); err != nil {
		return err
	}
	if err := command.Save(ctx, tx, result, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE favorite_delete_items`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE favorite_delete_folders`); err != nil {
		return err
	}
	return tx.Commit()
}

type favoriteDeleteItem struct {
	id        string
	version   int64
	updatedAt int64
}

// Keep application memory bounded even when the owner has many favorites.
func applyFavoriteCollectionDeletion(ctx context.Context, tx *sql.Tx, actor auth.User, action string, now time.Time) error {
	statement, err := tx.PrepareContext(ctx, `UPDATE favorites SET collection_id=NULL,favorited=?,updated_at=?,version=? WHERE user_id=? AND album_id=? AND version=?`)
	if err != nil {
		return err
	}
	defer statement.Close()
	favorited := action == FavoriteDeleteMoveToDefault
	op := "upsert"
	if !favorited {
		op = "delete"
	}
	var after *string
	for {
		query := `SELECT album_id,version,updated_at FROM favorite_delete_items ORDER BY album_id LIMIT 128`
		var args []any
		if after != nil {
			query = `SELECT album_id,version,updated_at FROM favorite_delete_items WHERE album_id>? ORDER BY album_id LIMIT 128`
			args = []any{*after}
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		page := make([]favoriteDeleteItem, 0, 128)
		for rows.Next() {
			var item favoriteDeleteItem
			if err := rows.Scan(&item.id, &item.version, &item.updatedAt); err != nil {
				rows.Close()
				return err
			}
			page = append(page, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, item := range page {
			updated := max(item.updatedAt, now.UnixMilli())
			instant := time.UnixMilli(updated).UTC()
			if instant.Year() < 1 || instant.Year() > 9999 {
				return errors.New("stored favorite update time is outside the public UTC range")
			}
			result, err := statement.ExecContext(ctx, favorited, updated, item.version+1, actor.ID, item.id, item.version)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count != 1 {
				return errors.New("confirmed favorite changed during deletion")
			}
			value := AlbumFavorite{AlbumID: item.id, Favorited: favorited, UpdatedAt: &instant, Version: item.version + 1}
			if err := store.Change(ctx, tx, actor.ScopeID, "favorite", item.id, op, value.Version, value, now); err != nil {
				return err
			}
		}
		after = &page[len(page)-1].id
	}
}

func journalDeletedFavoriteCollections(ctx context.Context, tx *sql.Tx, actor auth.User, now time.Time) error {
	var after *string
	for {
		query := `SELECT id,version FROM favorite_delete_folders ORDER BY id LIMIT 128`
		var args []any
		if after != nil {
			query = `SELECT id,version FROM favorite_delete_folders WHERE id>? ORDER BY id LIMIT 128`
			args = []any{*after}
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		page := make([]favoriteDeleteItem, 0, 128)
		for rows.Next() {
			var item favoriteDeleteItem
			if err := rows.Scan(&item.id, &item.version); err != nil {
				rows.Close()
				return err
			}
			page = append(page, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, item := range page {
			if !validFavoriteCollectionID(item.id) {
				return errors.New("stored collection identity is outside the public bound")
			}
			if err := store.Change(ctx, tx, actor.ScopeID, "favorite_collection", item.id, "delete", item.version+1, map[string]string{"id": item.id}, now); err != nil {
				return err
			}
		}
		after = &page[len(page)-1].id
	}
}
