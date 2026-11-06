package scanner

import (
	"context"
	"database/sql"
	"math"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
)

func (s *Inventory) publishMissing(ctx context.Context, tx *sql.Tx, lease jobs.Lease, generation Generation, source library.Source) error {
	after := ""
	full := generation.Mode == "full" && generation.scope == "."
	for {
		type missing struct {
			id, availability                    string
			since                               sql.NullInt64
			full, version                       int64
			mediaID, albumID, mediaAvailability sql.NullString
		}
		rows, err := tx.QueryContext(ctx, `SELECT e.id,e.availability,e.missing_since,e.missing_full_scans,e.version,m.id,m.album_id,m.availability
		 FROM source_entries e LEFT JOIN media m ON m.source_entry_id=e.id
		 WHERE e.source_id=? AND e.id>? AND NOT EXISTS(SELECT 1 FROM gg_publish_paths p WHERE p.entry_id=e.id)
		 AND (?='.' OR e.relative_path=? OR substr(e.relative_path,1,length(?)+1)=?||'/') ORDER BY e.id LIMIT ?`, source.ID, after, generation.scope, generation.scope, generation.scope, generation.scope, s.policy.BatchSize)
		if err != nil {
			return err
		}
		batch := make([]missing, 0, s.policy.BatchSize)
		for rows.Next() {
			var e missing
			if err := rows.Scan(&e.id, &e.availability, &e.since, &e.full, &e.version, &e.mediaID, &e.albumID, &e.mediaAvailability); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		var succeeded, skipped int64
		for _, e := range batch {
			now := s.clock()
			since, count := e.since, e.full
			if !since.Valid {
				since = sql.NullInt64{Int64: now.UnixMilli(), Valid: true}
			}
			if full {
				if count == math.MaxInt64 {
					return ErrLimit
				}
				count++
			}
			availability := "unavailable"
			if e.availability == "quarantined" || (count >= 2 && now.Sub(time.UnixMilli(since.Int64)) >= quarantineGrace) {
				availability = "quarantined"
			}
			changed := since != e.since || count != e.full || availability != e.availability
			if changed {
				e.version++
				if _, err := tx.ExecContext(ctx, `UPDATE source_entries SET availability=?,missing_since=?,missing_full_scans=?,version=?,updated_at=max(updated_at,?) WHERE id=?`, availability, since, count, e.version, now.UnixMilli(), e.id); err != nil {
					return err
				}
				if err := entryChange(ctx, tx, e.id, source.ID, availability, e.version, now); err != nil {
					return err
				}
			}
			if e.mediaID.Valid {
				if e.mediaAvailability.String != "archived" && e.mediaAvailability.String != availability {
					if _, err := tx.ExecContext(ctx, `UPDATE media SET availability=?,version=version+1,updated_at=max(updated_at,?) WHERE id=?`, availability, now.UnixMilli(), e.mediaID); err != nil {
						return err
					}
					if err := mediaChange(ctx, tx, e.mediaID.String, now); err != nil {
						return err
					}
					if err := dirtyAlbum(ctx, tx, e.albumID.String, false); err != nil {
						return err
					}
					changed = true
				}
				result, err := tx.ExecContext(ctx, `UPDATE albums SET library_id=? WHERE id=? AND library_id!=?`, source.LibraryID, e.albumID, source.LibraryID)
				if err != nil {
					return err
				}
				n, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if n > 0 {
					if err := dirtyAlbum(ctx, tx, e.albumID.String, false); err != nil {
						return err
					}
					changed = true
				}
			}
			if changed {
				succeeded++
			} else {
				skipped++
			}
		}
		if err := s.workProgress(ctx, tx, lease, succeeded, 0, skipped); err != nil {
			return err
		}
		if err := s.queue.HeartbeatTx(ctx, tx, lease); err != nil {
			return err
		}
		after = batch[len(batch)-1].id
	}
	return nil
}

func (s *Inventory) finishAlbums(ctx context.Context, tx *sql.Tx, lease jobs.Lease) error {
	after := ""
	for {
		type album struct {
			id          string
			quarantined sql.NullInt64
			version     int64
			isNew       bool
		}
		rows, err := tx.QueryContext(ctx, `SELECT a.id,a.quarantined_at,a.version,d.is_new FROM gg_publish_dirty_albums d JOIN albums a ON a.id=d.id WHERE a.id>? ORDER BY a.id LIMIT ?`, after, s.policy.BatchSize)
		if err != nil {
			return err
		}
		batch := make([]album, 0, s.policy.BatchSize)
		for rows.Next() {
			var a album
			if err := rows.Scan(&a.id, &a.quarantined, &a.version, &a.isNew); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, a)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, a := range batch {
			now := s.clock()
			var cover sql.NullString
			var allQuarantined bool
			if err := tx.QueryRowContext(ctx, `SELECT (SELECT m.id FROM media m JOIN albums owner ON owner.id=m.album_id WHERE m.album_id=a.id AND m.availability!='archived' AND COALESCE(json_extract(m.metadata_json,'$.status'),'unknown')!='not_media' ORDER BY CASE WHEN m.id=owner.cover_override_id THEN 0 WHEN m.id=owner.cover_media_id THEN 1 ELSE 2 END,m.id LIMIT 1),EXISTS(SELECT 1 FROM media m WHERE m.album_id=a.id AND m.availability!='archived') AND NOT EXISTS(SELECT 1 FROM media m WHERE m.album_id=a.id AND m.availability NOT IN ('quarantined','archived')) FROM albums a WHERE a.id=?`, a.id).Scan(&cover, &allQuarantined); err != nil {
				return err
			}
			quarantined := sql.NullInt64{}
			if allQuarantined {
				quarantined = a.quarantined
				if !quarantined.Valid {
					quarantined = sql.NullInt64{Int64: now.UnixMilli(), Valid: true}
				}
			}
			if !a.isNew {
				a.version++
			}
			if _, err := tx.ExecContext(ctx, `UPDATE albums SET cover_media_id=?,quarantined_at=?,version=?,updated_at=max(updated_at,?) WHERE id=?`, cover, quarantined, a.version, now.UnixMilli(), a.id); err != nil {
				return err
			}
			if err := access.JournalAlbumTx(ctx, tx, a.id, now); err != nil {
				return err
			}
		}
		after = batch[len(batch)-1].id
		if err := s.queue.HeartbeatTx(ctx, tx, lease); err != nil {
			return err
		}
	}
	return nil
}
