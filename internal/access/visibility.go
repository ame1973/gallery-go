package access

import (
	"context"

	"gallerygo/internal/auth"
)

// Predicates accept one viewer ID placeholder, read the role/active state from
// users, and require a session checked in the same transaction. Queries use the
// fixed aliases l (libraries), a (albums), and m (media). Never filter in memory.
const libraryGrant = `EXISTS (
 SELECT 1 FROM library_access la JOIN user_groups ug ON ug.group_id=la.group_id
 WHERE la.library_id=l.id AND ug.user_id=viewer.id)`

const LibraryVisible = `l.is_active=1 AND EXISTS (
 SELECT 1 FROM users viewer WHERE viewer.id=? AND viewer.active=1
 AND (viewer.role='admin' OR ` + libraryGrant + `))`

const AlbumVisible = `l.is_active=1
 AND (a.source_id IS NULL OR EXISTS (SELECT 1 FROM sources src WHERE src.id=a.source_id AND src.library_id=l.id))
 AND EXISTS (SELECT 1 FROM users viewer WHERE viewer.id=? AND viewer.active=1
 AND (viewer.role='admin' OR (` + libraryGrant + ` AND (
 a.access_restricted=0 OR EXISTS (
 SELECT 1 FROM album_access aa JOIN user_groups ug ON ug.group_id=aa.group_id
 WHERE aa.album_id=a.id AND ug.user_id=viewer.id)))))`

// A source reassignment must not expose old albums or media through the previous
// library's grants. Availability is separate: ready cached variants can remain
// readable during a source outage, but inconsistent source relationships deny all.
const MediaVisible = AlbumVisible + ` AND (m.source_entry_id IS NULL OR EXISTS (
 SELECT 1 FROM source_entries entry WHERE entry.id=m.source_entry_id AND entry.source_id=a.source_id))`

func (s *Service) Libraries(ctx context.Context, current auth.Session, after string, limit int) ([]Library, error) {
	if !pageLimit(limit) {
		return nil, ErrGroupSet
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, librarySelect+` WHERE `+LibraryVisible+` AND l.id>? ORDER BY l.id LIMIT ?`, viewer.ID, after, limit)
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

func (s *Service) Library(ctx context.Context, current auth.Session, id string) (Library, error) {
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return Library{}, err
	}
	defer tx.Rollback()
	return scanLibrary(tx.QueryRowContext(ctx, librarySelect+` WHERE `+LibraryVisible+` AND l.id=?`, viewer.ID, id))
}

type Album struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	LibraryID    string   `json:"library_id"`
	CoverMediaID *string  `json:"cover_media_id"`
	MediaCount   int64    `json:"media_count"`
	RatingAvg    *float64 `json:"rating_avg"`
	Version      int64    `json:"version"`
}

const albumSelect = `SELECT a.id,COALESCE(NULLIF(a.display_name,''),a.source_name),a.library_id,
 (SELECT m.id FROM media m WHERE m.id=a.cover_media_id AND m.album_id=a.id AND ` + MediaVisible + `),
 (SELECT COUNT(*) FROM media m WHERE m.album_id=a.id AND ` + MediaVisible + `),
 (SELECT AVG(r.rating) FROM ratings r WHERE r.album_id=a.id),a.version
 FROM albums a JOIN libraries l ON l.id=a.library_id`

func scanAlbum(row interface{ Scan(...any) error }) (Album, error) {
	var album Album
	err := row.Scan(&album.ID, &album.Title, &album.LibraryID, &album.CoverMediaID, &album.MediaCount, &album.RatingAvg, &album.Version)
	return album, err
}

func (s *Service) Albums(ctx context.Context, current auth.Session, after string, limit int) ([]Album, error) {
	if !pageLimit(limit) {
		return nil, ErrGroupSet
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, albumSelect+` WHERE `+AlbumVisible+` AND a.id>? ORDER BY a.id LIMIT ?`, viewer.ID, viewer.ID, viewer.ID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Album, 0)
	for rows.Next() {
		album, err := scanAlbum(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, album)
	}
	return items, rows.Err()
}

func (s *Service) Album(ctx context.Context, current auth.Session, id string) (Album, error) {
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return Album{}, err
	}
	defer tx.Rollback()
	return scanAlbum(tx.QueryRowContext(ctx, albumSelect+` WHERE `+AlbumVisible+` AND a.id=?`, viewer.ID, viewer.ID, viewer.ID, id))
}
