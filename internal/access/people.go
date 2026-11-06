package access

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
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

const personMetadataSelect = `SELECT p.id,p.source_id,p.source_name,p.display_name,p.version FROM people p`

var ErrPersonMetadata = errors.New("provide a display_name override")

type PersonMetadata struct {
	ID          string  `json:"id"`
	SourceID    string  `json:"source_id"`
	SourceName  string  `json:"source_name"`
	DisplayName *string `json:"display_name"`
	Version     int64   `json:"version"`
}

// Empty strings clear the override; nil means omitted and is not a valid command.
// HTTP null is rejected by the shared strict JSON decoder, not treated as a clear.
type PersonMetadataPatch struct {
	DisplayName *string `json:"display_name"`
}

func scanPersonMetadata(row interface{ Scan(...any) error }) (PersonMetadata, error) {
	var value PersonMetadata
	err := row.Scan(&value.ID, &value.SourceID, &value.SourceName, &value.DisplayName, &value.Version)
	return value, err
}

func (s *Service) AdminPersonMetadata(ctx context.Context, current auth.Session, id string) (PersonMetadata, error) {
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return PersonMetadata{}, err
	}
	defer tx.Rollback()
	return scanPersonMetadata(tx.QueryRowContext(ctx, personMetadataSelect+` WHERE `+PersonVisible+` AND p.id=?`, actor.ID, id))
}

func (s *Service) UpdatePersonMetadata(ctx context.Context, current auth.Session, id string, version int64, patch PersonMetadataPatch, key string) (PersonMetadata, error) {
	if patch.DisplayName == nil {
		return PersonMetadata{}, ErrPersonMetadata
	}
	if *patch.DisplayName != "" {
		name, err := normalizedName(*patch.DisplayName)
		if err != nil {
			return PersonMetadata{}, err
		}
		patch.DisplayName = &name
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return PersonMetadata{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(struct {
		ID      string              `json:"id"`
		Version int64               `json:"version"`
		Patch   PersonMetadataPatch `json:"patch"`
	}{id, version, patch})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "person.metadata.update", Metadata: string(data)}
	var result PersonMetadata
	if found, err := command.Replay(ctx, tx, now, &result); found || err != nil {
		return result, err
	}
	result, err = scanPersonMetadata(tx.QueryRowContext(ctx, personMetadataSelect+` WHERE `+PersonVisible+` AND p.id=?`, actor.ID, id))
	if err != nil {
		return PersonMetadata{}, err
	}
	if result.Version != version {
		return PersonMetadata{}, auth.ErrConflict
	}
	display := patch.DisplayName
	if *display == "" {
		display = nil
	}
	if !sameOverride(display, result.DisplayName) {
		if version == math.MaxInt64 {
			return PersonMetadata{}, auth.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE people SET display_name=?,version=version+1,updated_at=max(updated_at,?) WHERE id=?`, display, now.UnixMilli(), id); err != nil {
			return PersonMetadata{}, err
		}
		result.DisplayName = display
		result.Version++
		if err := store.Change(ctx, tx, "system", "person", id, "upsert", result.Version, map[string]any{"id": id, "source_id": result.SourceID, "source_name": result.SourceName, "display_name": result.DisplayName, "version": result.Version}, now); err != nil {
			return PersonMetadata{}, err
		}
		if err := store.Audit(ctx, tx, actor.ID, "person_metadata_updated", "person", id, map[string]any{"display_name": result.DisplayName, "version": result.Version}, now); err != nil {
			return PersonMetadata{}, err
		}
	}
	if err := command.Save(ctx, tx, result, now); err != nil {
		return PersonMetadata{}, err
	}
	return result, tx.Commit()
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
