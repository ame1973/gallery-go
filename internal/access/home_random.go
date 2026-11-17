package access

import (
	"context"
	"database/sql"
	"errors"

	"gallerygo/internal/auth"
)

// MaxRandomAlbums bounds a single unpaginated home sample.
const MaxRandomAlbums = 100

var ErrRandomAlbumLimit = errors.New("random album sample limit must be between 1 and 100")

// Materialize bounded IDs before projecting media counts and covers. Sampling
// scans candidate albums to apply visibility; it is not a cursor or indexed shuffle.
const randomAlbumsQuery = `WITH sampled AS MATERIALIZED (
 SELECT a.id,random() AS draw FROM albums a JOIN libraries l ON l.id=a.library_id
 WHERE ` + AlbumVisible + ` ORDER BY draw,a.id LIMIT ?
) ` + albumColumns + ` FROM sampled selected
 JOIN albums a ON a.id=selected.id` + albumRelations + `
 WHERE ` + AlbumVisible + ` ORDER BY selected.draw,selected.id`

func (s *Service) RandomAlbums(ctx context.Context, current auth.Session, limit int) ([]Album, error) {
	if limit < 1 || limit > MaxRandomAlbums {
		return nil, ErrRandomAlbumLimit
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	actor, err := s.auth.SessionInTx(ctx, tx, current)
	if err != nil {
		return nil, err
	}
	viewer := actor.User.ID
	rows, err := tx.QueryContext(ctx, randomAlbumsQuery, viewer, limit, viewer, viewer, viewer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Album, 0, limit)
	for rows.Next() {
		album, err := scanAlbum(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, album)
	}
	return items, rows.Err()
}
