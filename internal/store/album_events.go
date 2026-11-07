package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// MediaSourceConsistent uses aliases m/a; it is an identity fence, not a grant.
// Share it between authorized reads and the transaction-only canonical journal.
const MediaSourceConsistent = `m.availability!='archived' AND (m.source_entry_id IS NULL OR EXISTS (
 SELECT 1 FROM source_entries entry WHERE entry.id=m.source_entry_id AND entry.source_id=a.source_id))`

var ErrAlbumTagSet = errors.New("provide an explicit array of at most 100 unique tag IDs")

// AlbumTagIDsTx reads the bounded current association set without global labels.
func AlbumTagIDsTx(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT tag_id FROM album_tags WHERE album_id=? ORDER BY tag_id LIMIT 101`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) > 100 {
		return nil, ErrAlbumTagSet
	}
	return ids, rows.Err()
}

// JournalAlbumTx records one complete current shared album projection. It lives
// below auth/access because account deletion is also a rating aggregate producer.
// Callers authorize or fence the write and commit this journal with the mutation.
// Only transaction-bound DB reads occur; no source paths or files are read.
func JournalAlbumTx(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	var value struct {
		ID            string     `json:"id"`
		LibraryID     string     `json:"library_id"`
		SourceID      *string    `json:"source_id"`
		PersonID      *string    `json:"person_id"`
		SourceName    string     `json:"source_name"`
		DisplayName   *string    `json:"display_name"`
		CoverMediaID  *string    `json:"cover_media_id"`
		Restricted    bool       `json:"restricted"`
		QuarantinedAt *time.Time `json:"quarantined_at"`
		TagIDs        []string   `json:"tag_ids"`
		RatingAvg     *float64   `json:"rating_avg"`
		IndexedAt     time.Time  `json:"indexed_at"`
		Version       int64      `json:"version"`
	}
	var quarantine sql.NullInt64
	var indexed int64
	if err := tx.QueryRowContext(ctx, `SELECT a.id,a.library_id,a.source_id,a.person_id,a.source_name,a.display_name,
 (SELECT m.id FROM media m WHERE m.id=a.cover_media_id AND m.album_id=a.id AND `+MediaSourceConsistent+`),
 a.access_restricted,a.quarantined_at,a.rating_avg,a.created_at,a.version
 FROM albums a WHERE a.id=?`, id).Scan(&value.ID, &value.LibraryID, &value.SourceID, &value.PersonID,
		&value.SourceName, &value.DisplayName, &value.CoverMediaID, &value.Restricted, &quarantine, &value.RatingAvg, &indexed, &value.Version); err != nil {
		return err
	}
	value.IndexedAt = time.UnixMilli(indexed).UTC()
	if quarantine.Valid {
		instant := time.UnixMilli(quarantine.Int64).UTC()
		value.QuarantinedAt = &instant
	}
	var err error
	value.TagIDs, err = AlbumTagIDsTx(ctx, tx, id)
	if err != nil {
		return err
	}
	return Change(ctx, tx, "system", "album", id, "upsert", value.Version, value, now)
}
