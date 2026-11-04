package scanner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"path"
	"time"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

type publicationFile struct {
	file             File
	inspection       media.Inspection
	entryID, albumID sql.NullString
	isMedia          bool
}

func (s *Inventory) publishPaths(ctx context.Context, tx *sql.Tx, lease jobs.Lease, generation Generation, source library.Source) error {
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT f.relative_path,f.size_bytes,f.mtime_ns,f.file_identity,f.quick_fingerprint,f.sha256,md.inspection_json,p.entry_id,a.album_id,p.is_media
		 FROM gg_publish_paths p JOIN inventory_files f ON f.relative_path=p.path AND f.generation_id=?
		 JOIN inventory_media md ON md.generation_id=f.generation_id AND md.relative_path=f.relative_path
		 LEFT JOIN gg_publish_albums a ON a.path=p.album_path
		 WHERE p.selected=1 AND p.path>? ORDER BY p.path LIMIT ?`, generation.ID, after, s.policy.BatchSize)
		if err != nil {
			return err
		}
		batch := make([]publicationFile, 0, s.policy.BatchSize)
		for rows.Next() {
			var p publicationFile
			var data string
			if err := rows.Scan(&p.file.RelativePath, &p.file.SizeBytes, &p.file.MtimeNS, &p.file.Identity, &p.file.QuickFingerprint, &p.file.SHA256, &data, &p.entryID, &p.albumID, &p.isMedia); err != nil {
				rows.Close()
				return err
			}
			if err := json.Unmarshal([]byte(data), &p.inspection); err != nil {
				rows.Close()
				return err
			}
			if !validInspection(p.inspection) {
				rows.Close()
				return ErrGeneration
			}
			batch = append(batch, p)
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
		for _, p := range batch {
			entry, changed, err := s.publishEntry(ctx, tx, generation, source, p)
			if err != nil {
				return err
			}
			if p.isMedia {
				if !p.albumID.Valid {
					return ErrGeneration
				}
				mediaChanged, err := s.publishMedia(ctx, tx, p, entry)
				if err != nil {
					return err
				}
				changed = changed || mediaChanged
			}
			if changed {
				succeeded++
			} else {
				skipped++
			}
			if _, err := tx.ExecContext(ctx, `UPDATE gg_publish_paths SET entry_id=? WHERE path=?`, entry, p.file.RelativePath); err != nil {
				return err
			}
		}
		if err := s.workProgress(ctx, tx, lease, succeeded, 0, skipped); err != nil {
			return err
		}
		if err := s.queue.HeartbeatTx(ctx, tx, lease); err != nil {
			return err
		}
		after = batch[len(batch)-1].file.RelativePath
	}
	return nil
}

func (s *Inventory) publishEntry(ctx context.Context, tx *sql.Tx, generation Generation, source library.Source, p publicationFile) (string, bool, error) {
	now := s.clock()
	id := p.entryID.String
	version := int64(1)
	changed := true
	if !p.entryID.Valid {
		id = store.NewID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,quick_fingerprint,sha256,seen_generation,source_config_version,file_identity,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, source.ID, p.file.RelativePath, p.file.SizeBytes, p.file.MtimeNS, p.file.QuickFingerprint, p.file.SHA256, generation.Number, generation.ConfigVersion, p.file.Identity, now.UnixMilli()); err != nil {
			return "", false, err
		}
	} else {
		var relative, hash, quick, identity, availability string
		var size, mtime, config, missingFull int64
		var missingSince sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT relative_path,size_bytes,mtime_ns,COALESCE(quick_fingerprint,''),COALESCE(sha256,''),file_identity,availability,source_config_version,missing_since,missing_full_scans,version FROM source_entries WHERE id=? AND source_id=?`, id, source.ID).Scan(&relative, &size, &mtime, &quick, &hash, &identity, &availability, &config, &missingSince, &missingFull, &version); err != nil {
			return "", false, err
		}
		changed = relative != p.file.RelativePath || size != p.file.SizeBytes || mtime != p.file.MtimeNS || quick != p.file.QuickFingerprint || hash != p.file.SHA256 || identity != p.file.Identity || availability != "present" || config != generation.ConfigVersion || missingSince.Valid || missingFull != 0
		if changed {
			version++
			if _, err := tx.ExecContext(ctx, `UPDATE source_entries SET relative_path=?,size_bytes=?,mtime_ns=?,quick_fingerprint=?,sha256=?,file_identity=?,seen_generation=?,availability='present',source_config_version=?,missing_since=NULL,missing_full_scans=0,version=?,updated_at=max(updated_at,?) WHERE id=?`, p.file.RelativePath, p.file.SizeBytes, p.file.MtimeNS, p.file.QuickFingerprint, p.file.SHA256, p.file.Identity, generation.Number, generation.ConfigVersion, version, now.UnixMilli(), id); err != nil {
				return "", false, err
			}
		} else {
			// Like a lease heartbeat, private healthy-generation evidence does not
			// change the indexed resource version when all observable data is equal.
			if _, err := tx.ExecContext(ctx, `UPDATE source_entries SET seen_generation=? WHERE id=?`, generation.Number, id); err != nil {
				return "", false, err
			}
		}
	}
	if changed {
		if err := entryChange(ctx, tx, id, source.ID, "present", version, now); err != nil {
			return "", false, err
		}
	}
	return id, changed, nil
}

func (s *Inventory) publishMedia(ctx context.Context, tx *sql.Tx, p publicationFile, entryID string) (bool, error) {
	var id, albumID, kind, fileName, availability, metadata string
	var revision, sourceHash sql.NullString
	var sourceSize, sourceMtime sql.NullInt64
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT m.id,m.album_id,m.kind,m.file_name,m.availability,m.metadata_json,m.version,r.id,r.source_sha256,r.source_size,r.source_mtime_ns FROM media m LEFT JOIN media_revisions r ON r.id=m.current_revision_id AND r.media_id=m.id WHERE m.source_entry_id=?`, entryID).Scan(&id, &albumID, &kind, &fileName, &availability, &metadata, &version, &revision, &sourceHash, &sourceSize, &sourceMtime)
	isNew := errors.Is(err, sql.ErrNoRows)
	if err != nil && !isNew {
		return false, err
	}
	now := s.clock()
	data, err := json.Marshal(p.inspection)
	if err != nil {
		return false, err
	}
	desiredKind := p.inspection.Kind
	if desiredKind == "" {
		desiredKind = kind
	}
	desiredAvailability := "available"
	if p.inspection.Status == "not_media" {
		desiredAvailability = "unavailable"
	}
	if availability == "archived" {
		desiredAvailability = "archived"
	}
	if isNew {
		if desiredKind == "" {
			return false, ErrGeneration
		}
		id, version = store.NewID(), 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO media(id,album_id,source_entry_id,kind,file_name,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, p.albumID.String, entryID, desiredKind, path.Base(p.file.RelativePath), now.UnixMilli(), now.UnixMilli()); err != nil {
			return false, err
		}
	}
	newRevision := !revision.Valid || !sourceHash.Valid || sourceHash.String != p.file.SHA256 || !sourceSize.Valid || sourceSize.Int64 != p.file.SizeBytes || !sourceMtime.Valid || sourceMtime.Int64 != p.file.MtimeNS
	if newRevision {
		var number int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM media_revisions WHERE media_id=?`, id).Scan(&number); err != nil {
			return false, err
		}
		if number == math.MaxInt64 {
			return false, ErrLimit
		}
		number++
		revision = sql.NullString{String: store.NewID(), Valid: true}
		if _, err := tx.ExecContext(ctx, `INSERT INTO media_revisions(id,media_id,source_sha256,source_size,source_mtime_ns,version,created_at) VALUES(?,?,?,?,?,?,?)`, revision, id, p.file.SHA256, p.file.SizeBytes, p.file.MtimeNS, number, now.UnixMilli()); err != nil {
			return false, err
		}
		if err := store.Change(ctx, tx, "system", "media_revision", revision.String, "upsert", number, map[string]any{"id": revision.String, "media_id": id, "version": number, "source_sha256": p.file.SHA256, "source_size": p.file.SizeBytes}, now); err != nil {
			return false, err
		}
	}
	changed := isNew || newRevision || albumID != p.albumID.String || kind != desiredKind || fileName != path.Base(p.file.RelativePath) || availability != desiredAvailability || metadata != string(data)
	if !changed {
		return false, nil
	}
	if !isNew {
		version++
	}
	var captured any
	if p.inspection.CaptureAt != nil {
		captured = p.inspection.CaptureAt.UnixMilli()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE media SET album_id=?,kind=?,file_name=?,width=?,height=?,duration_ms=?,orientation=?,capture_at=?,mime=?,current_revision_id=?,availability=?,metadata_json=?,version=?,updated_at=max(updated_at,?) WHERE id=?`, p.albumID.String, desiredKind, path.Base(p.file.RelativePath), p.inspection.Width, p.inspection.Height, p.inspection.DurationMS, p.inspection.Orientation, captured, p.inspection.MIME, revision, desiredAvailability, string(data), version, now.UnixMilli(), id); err != nil {
		return false, err
	}
	if !isNew && albumID != p.albumID.String {
		if err := dirtyAlbum(ctx, tx, albumID, false); err != nil {
			return false, err
		}
	}
	if err := dirtyAlbum(ctx, tx, p.albumID.String, false); err != nil {
		return false, err
	}
	return true, mediaChange(ctx, tx, id, now)
}

func entryChange(ctx context.Context, tx *sql.Tx, id, source, availability string, version int64, now time.Time) error {
	return store.Change(ctx, tx, "system", "source_entry", id, "upsert", version, map[string]any{"id": id, "source_id": source, "availability": availability, "version": version}, now)
}

func mediaChange(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	var album, kind, fileName, availability, metadata string
	var revision sql.NullString
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT album_id,kind,file_name,availability,metadata_json,current_revision_id,version FROM media WHERE id=?`, id).Scan(&album, &kind, &fileName, &availability, &metadata, &revision, &version); err != nil {
		return err
	}
	var inspection media.Inspection
	if metadata != "{}" {
		if err := json.Unmarshal([]byte(metadata), &inspection); err != nil {
			return err
		}
	}
	return store.Change(ctx, tx, "system", "media", id, "upsert", version, map[string]any{"id": id, "album_id": album, "kind": kind, "file_name": fileName, "availability": availability, "inspection": inspection, "revision": nullableString(revision), "version": version}, now)
}
