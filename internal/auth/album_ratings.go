package auth

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"gallerygo/internal/store"
)

// deleteUserRatingsTx projects every removed live vote before the user cascade.
// A bounded-memory primary-key walk also covers albums the account can no longer
// access; deleting an account is not an authority to keep stale shared averages.
func deleteUserRatingsTx(ctx context.Context, tx *sql.Tx, user string, now time.Time) error {
	after := ""
	for {
		var id string
		var version int64
		err := tx.QueryRowContext(ctx, `SELECT r.album_id,a.version FROM ratings r JOIN albums a ON a.id=r.album_id
 WHERE r.user_id=? AND r.rating IS NOT NULL AND r.album_id>? ORDER BY r.album_id LIMIT 1`, user, after).Scan(&id, &version)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if version == math.MaxInt64 {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM ratings WHERE user_id=? AND album_id=?`, user, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE albums SET version=version+1,updated_at=max(updated_at,?) WHERE id=?`, now.UnixMilli(), id); err != nil {
			return err
		}
		if err := store.JournalAlbumTx(ctx, tx, id, now); err != nil {
			return err
		}
		after = id
	}
}
