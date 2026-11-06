package access

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gallerygo/internal/auth"
)

var (
	ErrAlbumSelection = errors.New("use bounded literal resource IDs and a supported album sort")
	ErrAlbumPosition  = errors.New("the album page position is invalid for this sort")
)

// Selectors intersect already-visible albums; they never grant access or resolve
// a global private catalog label. Empty fields mean omitted, not an HTTP value.
type AlbumFilter struct {
	LibraryID string
	SourceID  string
	PersonID  string
	TagID     string
	Sort      string
}

func (filter AlbumFilter) Validate() error {
	switch filter.Sort {
	case "", "latest", "oldest":
	default:
		return ErrAlbumSelection
	}
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

// Indexed is the public indexed_at UTC-millisecond key, never a source timestamp.
// Freeze it when returning a page; do not resolve a mutable/deleted anchor row.
type AlbumPosition struct {
	ID      string
	Indexed string
}

func (position AlbumPosition) indexedKey(filter AlbumFilter) (int64, error) {
	if len(position.ID) > 64 || !utf8.ValidString(position.ID) || len(position.Indexed) > 32 || ((position.ID == "" || filter.Sort == "") && position.Indexed != "") {
		return 0, ErrAlbumPosition
	}
	if position.ID == "" || filter.Sort == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(position.Indexed, 10, 64)
	if err != nil || strconv.FormatInt(value, 10) != position.Indexed {
		return 0, ErrAlbumPosition
	}
	instant := time.UnixMilli(value).UTC()
	if instant.Year() < 1 || instant.Year() > 9999 {
		return 0, ErrAlbumPosition
	}
	return value, nil
}

func albumSelection(viewer string, filter AlbumFilter, after AlbumPosition, indexedKey int64, limit int) (string, []any) {
	from, key, relations := ` FROM albums a`, `a.id`, albumRelations
	if filter.Sort != "" {
		index := "idx_album_indexed_page"
		switch {
		case filter.PersonID != "":
			index = "idx_album_person_indexed_page"
		case filter.SourceID != "":
			index = "idx_album_source_indexed_page"
		case filter.LibraryID != "":
			index = "idx_album_library_indexed_page"
		}
		// Finite compiled index/order fragments only. Chronological tag selection
		// probes an indexed edge from this ordered driver, not an ID-sorted edge.
		from += ` INDEXED BY ` + index
	} else {
		switch {
		case filter.TagID != "":
			// Keep sparse ID-only reverse-tag keysets outermost; ordinary joins can
			// reorder combined filters and sort the whole result instead.
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
	}
	query := albumColumns + from + relations + ` WHERE ` + AlbumVisible
	arguments := []any{viewer, viewer, viewer}
	if filter.TagID != "" {
		if filter.Sort == "" {
			query += ` AND selection.tag_id=?`
		} else {
			query += ` AND EXISTS(SELECT 1 FROM album_tags selection WHERE selection.album_id=a.id AND selection.tag_id=?)`
		}
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
		// The visible outer album supplies PersonVisible's album proof. Still
		// require the same concrete source/path, not a stale cross-source link.
		query += ` AND a.person_id=? AND EXISTS(SELECT 1 FROM people p WHERE ` + personIdentity + ` AND ` + personAlbumRelation + `)`
		arguments = append(arguments, filter.PersonID)
	}
	if filter.Sort == "" {
		query += ` AND ` + key + `>? ORDER BY ` + key
		arguments = append(arguments, after.ID)
	} else {
		comparison, direction := ">", "ASC"
		if filter.Sort == "latest" {
			comparison, direction = "<", "DESC"
		}
		if after.ID != "" {
			query += ` AND (a.created_at,a.id)` + comparison + `(?,?)`
			arguments = append(arguments, indexedKey, after.ID)
		}
		query += ` ORDER BY a.created_at ` + direction + `,a.id ` + direction
	}
	return query + ` LIMIT ?`, append(arguments, limit)
}

func (s *Service) Albums(ctx context.Context, current auth.Session, filter AlbumFilter, after AlbumPosition, limit int) ([]Album, error) {
	if !pageLimit(limit) {
		return nil, ErrGroupSet
	}
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	indexedKey, err := after.indexedKey(filter)
	if err != nil {
		return nil, err
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	query, arguments := albumSelection(viewer.ID, filter, after, indexedKey, limit)
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
