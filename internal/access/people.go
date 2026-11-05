package access

import (
	"context"

	"gallerygo/internal/auth"
)

// PersonVisible uses outer alias p and the same fresh-session album predicate as
// every other resource read. Indexed people bind to a source/path identity, not a
// display label. A cross-source album link must not reveal another person's name.
const personAlbumRelation = `a.person_id=p.id AND a.source_id=p.source_id`
const PersonVisible = `p.source_id IS NOT NULL AND p.relative_path IS NOT NULL
 AND length(p.relative_path)>0 AND p.relative_path!='.' AND EXISTS (
 SELECT 1 FROM albums a JOIN libraries l ON l.id=a.library_id
 WHERE ` + personAlbumRelation + ` AND ` + AlbumVisible + `)`

type Person struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	SourceID     string  `json:"source_id"`
	AlbumCount   int64   `json:"album_count"`
	CoverMediaID *string `json:"cover_media_id"`
	Version      int64   `json:"version"`
}

const personSelect = `SELECT p.id,COALESCE(NULLIF(p.display_name,''),p.source_name),p.source_id,
 (SELECT COUNT(*) FROM albums a JOIN libraries l ON l.id=a.library_id
  WHERE ` + personAlbumRelation + ` AND ` + AlbumVisible + `),
 (SELECT m.id FROM media m JOIN albums a ON a.id=m.album_id JOIN libraries l ON l.id=a.library_id
  WHERE ` + personAlbumRelation + ` AND ` + MediaVisible + `
  ORDER BY CASE WHEN m.id=a.cover_media_id THEN 0 ELSE 1 END,m.id LIMIT 1),p.version
 FROM people p`

func scanPerson(row interface{ Scan(...any) error }) (Person, error) {
	var person Person
	err := row.Scan(&person.ID, &person.Name, &person.SourceID, &person.AlbumCount, &person.CoverMediaID, &person.Version)
	return person, err
}

func (s *Service) People(ctx context.Context, current auth.Session, after string, limit int) ([]Person, error) {
	if !pageLimit(limit) {
		return nil, ErrGroupSet
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, personSelect+` WHERE `+PersonVisible+` AND p.id>? ORDER BY p.id LIMIT ?`, viewer.ID, viewer.ID, viewer.ID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Person{}
	for rows.Next() {
		person, err := scanPerson(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, person)
	}
	return items, rows.Err()
}

func (s *Service) Person(ctx context.Context, current auth.Session, id string) (Person, error) {
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return Person{}, err
	}
	defer tx.Rollback()
	return scanPerson(tx.QueryRowContext(ctx, personSelect+` WHERE `+PersonVisible+` AND p.id=?`, viewer.ID, viewer.ID, viewer.ID, id))
}

func (s *Service) PersonAlbums(ctx context.Context, current auth.Session, personID, after string, limit int) ([]Album, error) {
	if !pageLimit(limit) {
		return nil, ErrGroupSet
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT p.id FROM people p WHERE `+PersonVisible+` AND p.id=?`, viewer.ID, personID).Scan(&id); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, albumSelect+` WHERE `+AlbumVisible+`
 AND a.person_id=? AND a.source_id=(SELECT source_id FROM people WHERE id=?)
 AND a.id>? ORDER BY a.id LIMIT ?`, viewer.ID, viewer.ID, viewer.ID, personID, personID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Album{}
	for rows.Next() {
		album, err := scanAlbum(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, album)
	}
	return items, rows.Err()
}
