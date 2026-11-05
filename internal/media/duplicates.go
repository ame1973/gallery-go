package media

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
)

var ErrDuplicateQuery = errors.New("use a supported duplicate selection and bounded page limit")

type DuplicateFilter struct {
	LibraryID string
	SourceID  string
}

func (filter DuplicateFilter) Validate() error {
	for _, value := range []string{filter.LibraryID, filter.SourceID} {
		if len(value) > 64 || !utf8.ValidString(value) || strings.ContainsAny(value, "/\\") || strings.TrimSpace(value) != value {
			return ErrDuplicateQuery
		}
		for _, character := range value {
			if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
				return ErrDuplicateQuery
			}
		}
	}
	return nil
}

// ExactDuplicateGroup is indexed equality, never perceptual similarity or proof
// that an original is still reachable/unchanged. IDs are immutable representative
// revision IDs, not a raw source digest or a mutable logical media identifier.
type ExactDuplicateGroup struct {
	ID         string `json:"id"`
	SourceSize int64  `json:"source_size"`
	MediaCount int64  `json:"media_count"`
}

const duplicateFilters = ` AND (?='' OR l.id=?) AND (?='' OR a.source_id=?)`

// SQLite text length/GLOB stop at NUL. Require exact byte length as well, so a
// valid-looking digest prefix with a hidden tail can never form a trusted group.
const duplicateSignature = `typeof(r.source_size)='integer' AND r.source_size>0
 AND typeof(r.source_sha256)='text' AND length(r.source_sha256)=64
 AND length(CAST(r.source_sha256 AS BLOB))=64 AND r.source_sha256 NOT GLOB '*[^0-9a-f]*'`
const duplicateCorpus = `SELECT m.id AS media_id,r.id AS revision_id,r.source_sha256,r.source_size
 FROM media m JOIN media_revisions r ON r.id=m.current_revision_id AND r.media_id=m.id
 JOIN albums a ON a.id=m.album_id JOIN libraries l ON l.id=a.library_id
 WHERE ` + access.MediaVisible + ` AND ` + duplicateSignature + duplicateFilters

func duplicateArguments(viewer string, filter DuplicateFilter) []any {
	return []any{viewer, filter.LibraryID, filter.LibraryID, filter.SourceID, filter.SourceID}
}

func (s *Service) ExactDuplicates(ctx context.Context, current auth.Session, filter DuplicateFilter, after string, limit int) ([]ExactDuplicateGroup, error) {
	if err := filter.Validate(); err != nil || limit < 1 || limit > 101 {
		return nil, ErrDuplicateQuery
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if viewer.Role != "admin" {
		return nil, auth.ErrForbidden
	}
	arguments := append(duplicateArguments(viewer.ID, filter), after, limit)
	// Apply the keyset AFTER grouping: filtering individual revisions by cursor
	// first would silently turn a duplicate group into a singleton on later pages.
	rows, err := tx.QueryContext(ctx, `WITH corpus AS (`+duplicateCorpus+`)
 SELECT MIN(revision_id),source_size,COUNT(*) FROM corpus
 GROUP BY source_sha256,source_size HAVING COUNT(*)>1 AND MIN(revision_id)>?
 ORDER BY MIN(revision_id) LIMIT ?`, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ExactDuplicateGroup{}
	for rows.Next() {
		var item ExactDuplicateGroup
		if err := rows.Scan(&item.ID, &item.SourceSize, &item.MediaCount); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) ExactDuplicateMedia(ctx context.Context, current auth.Session, groupID string, filter DuplicateFilter, after string, limit int) ([]Metadata, error) {
	if err := filter.Validate(); err != nil || limit < 1 || limit > 101 || len(groupID) > 64 {
		return nil, ErrDuplicateQuery
	}
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if viewer.Role != "admin" {
		return nil, auth.ErrForbidden
	}
	var hash string
	var size int64
	arguments := append(duplicateArguments(viewer.ID, filter), groupID)
	// Resolve membership and the current representative inside the SAME session/
	// visibility transaction. A rewritten/hidden/removed anchor cannot retarget an
	// existing member-page cursor to a different content signature.
	err = tx.QueryRowContext(ctx, `WITH corpus AS (`+duplicateCorpus+`), groups AS (
 SELECT MIN(revision_id) AS id,source_sha256,source_size FROM corpus
 GROUP BY source_sha256,source_size HAVING COUNT(*)>1)
 SELECT source_sha256,source_size FROM groups WHERE id=?`, arguments...).Scan(&hash, &size)
	if err != nil {
		return nil, err
	}
	arguments = append(duplicateArguments(viewer.ID, filter), hash, size, after, limit)
	rows, err := tx.QueryContext(ctx, mediaSelect+` WHERE `+access.MediaVisible+duplicateFilters+` AND `+duplicateSignature+`
 AND r.source_sha256=? AND r.source_size=? AND m.id>? ORDER BY m.id LIMIT ?`, arguments...)
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
