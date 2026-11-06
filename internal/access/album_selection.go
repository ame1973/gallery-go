package access

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"gallerygo/internal/auth"
)

var ErrAlbumSelection = errors.New("use bounded literal library, source, person or tag selection IDs")

// Selectors intersect already-visible albums; they never grant access or resolve
// a global private catalog label. Empty fields mean omitted, not an HTTP value.
type AlbumFilter struct {
	LibraryID string
	SourceID  string
	PersonID  string
	TagID     string
}

func (filter AlbumFilter) Validate() error {
	for _, id := range []string{filter.LibraryID, filter.SourceID, filter.PersonID, filter.TagID} {
		if len(id) > 64 || !utf8.ValidString(id) || strings.TrimSpace(id) != id || strings.ContainsAny(id, "/\\") {
			return ErrAlbumSelection
		}
		for _, character := range id {
			if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
				return ErrAlbumSelection
			}
		}
	}
	return nil
}

func albumSelection(viewer string, filter AlbumFilter, after string, limit int) (string, []any) {
	from, key, relations := ` FROM albums a`, `a.id`, albumRelations
	switch {
	case filter.TagID != "":
		// The reverse edge is already indexed by tag+album, including sparse tags.
		// Actual combined-filter plans otherwise reorder to person/library first
		// and sort the entire result. Keep sparse reverse-tag keysets outermost.
		from = ` FROM album_tags selection INDEXED BY idx_album_tag_reverse CROSS JOIN albums a ON a.id=selection.album_id`
		relations = ` CROSS JOIN libraries l ON l.id=a.library_id`
		key = `selection.album_id`
	case filter.PersonID != "":
		from += ` INDEXED BY idx_album_person`
	case filter.SourceID != "":
		from += ` INDEXED BY idx_album_source_page`
	case filter.LibraryID != "":
		from += ` INDEXED BY idx_albums_library`
	}
	query := albumColumns + from + relations + ` WHERE ` + AlbumVisible
	arguments := []any{viewer, viewer, viewer}
	if filter.TagID != "" {
		query += ` AND selection.tag_id=?`
		arguments = append(arguments, filter.TagID)
	}
	if filter.LibraryID != "" {
		query += ` AND a.library_id=?`
		arguments = append(arguments, filter.LibraryID)
	}
	if filter.SourceID != "" {
		query += ` AND a.source_id=?`
		arguments = append(arguments, filter.SourceID)
	}
	if filter.PersonID != "" {
		// The visible outer album already supplies PersonVisible's album proof.
		// Still require the same concrete indexed source/path, never only a label
		// or a stale cross-source person_id foreign key.
		query += ` AND a.person_id=? AND EXISTS(SELECT 1 FROM people p WHERE ` + personIdentity + ` AND ` + personAlbumRelation + `)`
		arguments = append(arguments, filter.PersonID)
	}
	query += ` AND ` + key + `>? ORDER BY ` + key + ` LIMIT ?`
	return query, append(arguments, after, limit)
}

func (s *Service) Albums(ctx context.Context, current auth.Session, filter AlbumFilter, after string, limit int) ([]Album, error) {
	if !pageLimit(limit) {
		return nil, ErrGroupSet
	}
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	if len(after) > 64 || !utf8.ValidString(after) {
		return nil, ErrAlbumSelection
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	query, arguments := albumSelection(viewer.ID, filter, after, limit)
	rows, err := tx.QueryContext(ctx, query, arguments...)
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
