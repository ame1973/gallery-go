package auth

import (
	"context"
	"database/sql"
	"math"
	"time"

	"gallerygo/internal/store"
)

const (
	userDeleteRoutingSQL = `SELECT EXISTS(SELECT 1 FROM album_views WHERE user_id=?),
 (SELECT COUNT(*) FROM (SELECT 1 FROM ratings WHERE user_id=? AND rating IS NOT NULL LIMIT 129))`
	userDeleteStageRatingsSQL = `INSERT INTO temp.deleting_user_album_ids SELECT album_id FROM ratings WHERE user_id=? AND rating IS NOT NULL`
	userDeleteStageViewsSQL   = `INSERT OR IGNORE INTO temp.deleting_user_album_ids SELECT album_id FROM album_views WHERE user_id=?`
)

// deleteUserInteractionsTx removes personal views and live votes together, then
// journals each affected album once, including albums the account cannot see.
// Small accounts without views reuse the existing rating-only path. Otherwise
// SQLite holds the affected IDs; the Go journal walk retains at most one page.
// ponytail: the 128-row crossover selects a path, not an account size limit.
func deleteUserInteractionsTx(ctx context.Context, tx *sql.Tx, user string, now time.Time) error {
	var hasViews bool
	var ratings int
	if err := tx.QueryRowContext(ctx, userDeleteRoutingSQL, user, user).Scan(&hasViews, &ratings); err != nil {
		return err
	}
	if !hasViews && ratings <= 128 {
		return deleteUserRatingsTx(ctx, tx, user, now)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE deleting_user_album_ids(album_id TEXT PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, userDeleteStageRatingsSQL, user); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, userDeleteStageViewsSQL, user); err != nil {
		return err
	}
	var overflow bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM temp.deleting_user_album_ids d JOIN albums a ON a.id=d.album_id WHERE a.version=?)`, int64(math.MaxInt64)).Scan(&overflow); err != nil {
		return err
	}
	if overflow {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE albums SET version=version+1,updated_at=max(updated_at,?) WHERE id IN (SELECT album_id FROM temp.deleting_user_album_ids)`, now.UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ratings WHERE user_id=? AND rating IS NOT NULL`, user); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM album_views WHERE user_id=?`, user); err != nil {
		return err
	}
	first, after := true, ""
	for {
		query, args := `SELECT album_id FROM temp.deleting_user_album_ids WHERE album_id>? ORDER BY album_id LIMIT 128`, []any{after}
		if first {
			query, args, first = `SELECT album_id FROM temp.deleting_user_album_ids ORDER BY album_id LIMIT 128`, nil, false
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		ids := make([]string, 0, 128)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := store.JournalAlbumBatchTx(ctx, tx, ids, now); err != nil {
			return err
		}
		if len(ids) < 128 {
			break
		}
		after = ids[len(ids)-1]
	}
	_, err := tx.ExecContext(ctx, `DROP TABLE temp.deleting_user_album_ids`)
	return err
}
