package media

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
)

var (
	ErrMediaSelection = errors.New("use a supported media kind, capture interval and sort")
	ErrMediaPosition  = errors.New("the media page position is invalid for this sort")
)

// Capture filters describe proven capture instants, never filesystem timestamps.
// The range is inclusive/exclusive and may use any explicit RFC3339 offset.
type AlbumMediaFilter struct {
	Kind           string
	CapturedFrom   *time.Time
	CapturedBefore *time.Time
	Sort           string
}

func (filter AlbumMediaFilter) Validate() error {
	if filter.Kind != "" && filter.Kind != "image" && filter.Kind != "video" {
		return ErrMediaSelection
	}
	switch filter.Sort {
	case "", "id_asc", "id_desc", "captured_asc", "captured_desc":
	default:
		return ErrMediaSelection
	}
	for _, instant := range []*time.Time{filter.CapturedFrom, filter.CapturedBefore} {
		if instant != nil && (instant.UTC().Year() < 1 || instant.UTC().Year() > 9999 || instant.Nanosecond()%int(time.Millisecond) != 0) {
			return ErrMediaSelection
		}
	}
	if filter.CapturedFrom != nil && filter.CapturedBefore != nil && !filter.CapturedFrom.Before(*filter.CapturedBefore) {
		return ErrMediaSelection
	}
	return nil
}

// Capture is a public UTC-millisecond key or "unknown", not a source/stat token.
// The key is captured when returning the page; no mutable anchor row is resolved.
type AlbumMediaPosition struct {
	ID      string
	Capture string
}

func (position AlbumMediaPosition) captureKey(filter AlbumMediaFilter) (int64, error) {
	captured := strings.HasPrefix(filter.Sort, "captured_")
	if len(position.ID) > 64 || !utf8.ValidString(position.ID) || len(position.Capture) > 32 || ((position.ID == "" || !captured) && position.Capture != "") {
		return 0, ErrMediaPosition
	}
	if position.ID == "" || !captured {
		return 0, nil
	}
	if position.Capture == "unknown" {
		return math.MinInt64, nil
	}
	value, err := strconv.ParseInt(position.Capture, 10, 64)
	if err != nil || strconv.FormatInt(value, 10) != position.Capture {
		return 0, ErrMediaPosition
	}
	instant := time.UnixMilli(value).UTC()
	if instant.Year() < 1 || instant.Year() > 9999 {
		return 0, ErrMediaPosition
	}
	return value, nil
}

// Keep this expression identical to migration0020. The sentinel is below every
// supported capture instant, making NULL a total-order key without inventing time.
const mediaCaptureKey = `COALESCE(m.capture_at,-9223372036854775808)`

func albumMediaSelection(viewer, album string, filter AlbumMediaFilter, after AlbumMediaPosition, captureKey int64, limit int) (string, []any) {
	captured := strings.HasPrefix(filter.Sort, "captured_")
	index, key := "idx_media_album", "m.id"
	if captured {
		index, key = "idx_media_album_capture", mediaCaptureKey
	}
	if filter.Kind != "" {
		index = "idx_media_album_kind"
		if captured {
			index = "idx_media_album_kind_capture"
		}
	}
	// These index/order fragments come only from the compiled finite selection.
	// Force the ordered index, not a full-result temporary sort chosen for a filter.
	query := mediaColumns + ` FROM media m INDEXED BY ` + index + mediaRelations + ` WHERE ` + access.MediaVisible + ` AND m.album_id=?`
	arguments := []any{viewer, album}
	if filter.Kind != "" {
		query += ` AND m.kind=?`
		arguments = append(arguments, filter.Kind)
	}
	if filter.CapturedFrom != nil || filter.CapturedBefore != nil {
		from := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
		if filter.CapturedFrom != nil {
			from = filter.CapturedFrom.UnixMilli()
		}
		query += ` AND ` + mediaCaptureKey + `>=?`
		arguments = append(arguments, from)
		if filter.CapturedBefore != nil {
			query += ` AND ` + mediaCaptureKey + `<?`
			arguments = append(arguments, filter.CapturedBefore.UnixMilli())
		}
	}
	comparison, direction := ">", "ASC"
	if filter.Sort == "id_desc" || filter.Sort == "captured_desc" {
		comparison, direction = "<", "DESC"
	}
	if after.ID != "" {
		if captured {
			query += ` AND (` + key + `,m.id)` + comparison + `(?,?)`
			arguments = append(arguments, captureKey, after.ID)
		} else {
			query += ` AND m.id` + comparison + `?`
			arguments = append(arguments, after.ID)
		}
	}
	query += ` ORDER BY ` + key + ` ` + direction
	if captured {
		query += `,m.id ` + direction
	}
	query += ` LIMIT ?`
	return query, append(arguments, limit)
}

func (s *Service) AlbumMedia(ctx context.Context, current auth.Session, albumID string, filter AlbumMediaFilter, after AlbumMediaPosition, limit int) ([]Metadata, error) {
	if limit < 1 || limit > 101 {
		return nil, access.ErrGroupSet
	}
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	captureKey, err := after.captureKey(filter)
	if err != nil {
		return nil, err
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT a.id FROM albums a JOIN libraries l ON l.id=a.library_id WHERE `+access.AlbumVisible+` AND a.id=?`, viewer.ID, albumID).Scan(&id); err != nil {
		return nil, err
	}
	query, arguments := albumMediaSelection(viewer.ID, albumID, filter, after, captureKey, limit)
	rows, err := tx.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Metadata{}
	for rows.Next() {
		item, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
