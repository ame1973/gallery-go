package access

import (
	"context"
	"encoding/base64"
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

// Display overrides are at most 400 bytes; indexed source basenames are bounded by
// the scanner's 4096-byte relative path, not an assumed OS 255-byte component limit.
const MaxAlbumTitleBytes = 4096

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
	case "", "latest", "oldest", "name_asc", "name_desc":
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

// Key freezes a public UTC-millisecond indexed_at or a framed literal title. Never
// resolve a mutable/deleted anchor or hash/truncate titles into a different order.
type AlbumPosition struct {
	ID  string
	Key string
}

// Encoding the exact public title avoids JSON control/HTML-escaping expansion and
// distinguishes an empty title from a missing position. This is not a capability.
func AlbumNameKey(title string) string {
	return "n:" + base64.RawURLEncoding.EncodeToString([]byte(title))
}

func (position AlbumPosition) orderKey(filter AlbumFilter) (any, error) {
	if len(position.ID) > 64 || !utf8.ValidString(position.ID) || ((position.ID == "" || filter.Sort == "") && position.Key != "") {
		return nil, ErrAlbumPosition
	}
	if position.ID == "" || filter.Sort == "" {
		return int64(0), nil
	}
	if strings.HasPrefix(filter.Sort, "name_") {
		if len(position.Key) > 2+base64.RawURLEncoding.EncodedLen(MaxAlbumTitleBytes) || !strings.HasPrefix(position.Key, "n:") {
			return nil, ErrAlbumPosition
		}
		data, err := base64.RawURLEncoding.Strict().DecodeString(position.Key[2:])
		title := string(data)
		if err != nil || len(title) > MaxAlbumTitleBytes || !utf8.ValidString(title) || AlbumNameKey(title) != position.Key {
			return nil, ErrAlbumPosition
		}
		return title, nil
	}
	if len(position.Key) > 32 {
		return nil, ErrAlbumPosition
	}
	value, err := strconv.ParseInt(position.Key, 10, 64)
	if err != nil || strconv.FormatInt(value, 10) != position.Key {
		return nil, ErrAlbumPosition
	}
	instant := time.UnixMilli(value).UTC()
	if instant.Year() < 1 || instant.Year() > 9999 {
		return nil, ErrAlbumPosition
	}
	return value, nil
}

func albumSelection(viewer string, filter AlbumFilter, after AlbumPosition, sortKey any, limit int) (string, []any) {
	from, key, relations := ` FROM albums a`, `a.id`, albumRelations
	named := strings.HasPrefix(filter.Sort, "name_")
	if filter.Sort != "" {
		orderIndex, prefix := "indexed", ""
		if named {
			orderIndex = "title"
		}
		switch {
		case filter.PersonID != "":
			prefix = "person_"
		case filter.SourceID != "":
			prefix = "source_"
		case filter.LibraryID != "":
			prefix = "library_"
		}
		// Only finite compiled fragments enter the index/order vocabulary. Ordered
		// tag selection probes an indexed edge, never an ID-sorted tag driver.
		from += ` INDEXED BY idx_album_` + prefix + orderIndex + `_page`
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
		comparison, direction, orderKey := ">", "ASC", `a.created_at`
		if named {
			orderKey = albumTitle + ` COLLATE BINARY`
		}
		if filter.Sort == "latest" || filter.Sort == "name_desc" {
			comparison, direction = "<", "DESC"
		}
		if after.ID != "" {
			query += ` AND (` + orderKey + `,a.id)` + comparison + `(?,?)`
			arguments = append(arguments, sortKey, after.ID)
		}
		query += ` ORDER BY ` + orderKey + ` ` + direction + `,a.id ` + direction
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
	sortKey, err := after.orderKey(filter)
	if err != nil {
		return nil, err
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	query, arguments := albumSelection(viewer.ID, filter, after, sortKey, limit)
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
