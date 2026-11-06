package scanner

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

const quarantineGrace = 24 * time.Hour

// Publish revalidates the complete source before opening the writer. Planning,
// identities, revisions, missing evidence, events, source/generation publication,
// and job completion then commit together. No filesystem work runs in that writer.
func (s *Inventory) Publish(ctx context.Context, lease jobs.Lease, id string) (Generation, error) {
	return s.publish(ctx, lease, id, true)
}

// PublishPart commits one source of a multi-source job without inventing a known
// total or terminal result. The coordinator must finish the remaining sources and
// finalize the job; a single successful part is never an all-source success.
func (s *Inventory) PublishPart(ctx context.Context, lease jobs.Lease, id string) (Generation, error) {
	return s.publish(ctx, lease, id, false)
}

func (s *Inventory) publish(ctx context.Context, lease jobs.Lease, id string, final bool) (Generation, error) {
	ctx, cancel := context.WithTimeout(ctx, s.policy.Timeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Generation{}, err
	}
	generation, source, root, err := s.RequireMetadataTx(ctx, tx, lease, id)
	tx.Rollback()
	if err != nil {
		return Generation{}, err
	}
	if err := s.verifyManifest(ctx, generation, source, root); err != nil {
		return Generation{}, err
	}
	return s.publishVerified(ctx, lease, generation, source.Version, final)
}

// publishVerified is the DB-only half of publication. A newer source health
// mutation cannot be overwritten with evidence captured before that mutation.
func (s *Inventory) publishVerified(ctx context.Context, lease jobs.Lease, generation Generation, sourceVersion int64, final bool) (Generation, error) {
	id := generation.ID
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Generation{}, err
	}
	defer tx.Rollback()
	current, source, _, err := s.RequireMetadataTx(ctx, tx, lease, id)
	if err != nil {
		return Generation{}, err
	}
	if current.Version != generation.Version {
		return Generation{}, ErrGeneration
	}
	if source.Version != sourceVersion {
		return Generation{}, library.ErrSourceChanged
	}
	if err := s.queue.HeartbeatTx(ctx, tx, lease); err != nil {
		return Generation{}, err
	}
	if err := s.publicationPlan(ctx, tx, lease, current, source); err != nil {
		return Generation{}, err
	}
	var diagnosticWork int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory_issues WHERE generation_id=? AND error_code IN ('AMBIGUOUS_RENAME','AMBIGUOUS_ALBUM_RENAME','TARGET_RENAME_OUTSIDE_SCOPE')`, id).Scan(&diagnosticWork); err != nil {
		return Generation{}, err
	}
	if diagnosticWork > 0 {
		if err := s.workProgress(ctx, tx, lease, 0, diagnosticWork, 0); err != nil {
			return Generation{}, err
		}
	}
	var ownershipChanged bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM albums a WHERE a.source_id=? AND a.library_id!=?) OR EXISTS(SELECT 1 FROM gg_publish_paths p JOIN media m ON m.source_entry_id=p.entry_id JOIN gg_publish_albums a ON a.path=p.album_path WHERE p.selected=1 AND (a.album_id IS NULL OR a.album_id!=m.album_id))`, source.ID, source.LibraryID).Scan(&ownershipChanged); err != nil {
		return Generation{}, err
	}
	if ownershipChanged {
		if err := access.RevokeInventoryScopesTx(ctx, tx, source.ID, s.clock()); err != nil {
			return Generation{}, err
		}
	}
	if err := s.publishPeople(ctx, tx, lease, source); err != nil {
		return Generation{}, err
	}
	if err := s.publishAlbums(ctx, tx, lease, source, ownershipChanged); err != nil {
		return Generation{}, err
	}
	if err := s.rehomeRetainedAlbums(ctx, tx, lease, current, source); err != nil {
		return Generation{}, err
	}
	if err := s.publishPaths(ctx, tx, lease, current, source); err != nil {
		return Generation{}, err
	}
	if err := s.publishMissing(ctx, tx, lease, current, source); err != nil {
		return Generation{}, err
	}
	if err := s.finishAlbums(ctx, tx, lease); err != nil {
		return Generation{}, err
	}
	if _, err := s.sources.PublishInventoryTx(ctx, tx, source.ID, current.ConfigVersion, current.Number, current.Mode == "full" && current.scope == "."); err != nil {
		return Generation{}, err
	}
	now := s.clock()
	current.State, current.PublishedAt = "published", &now
	if err := s.save(ctx, tx, &current, now); err != nil {
		return Generation{}, err
	}
	var issues int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory_issues WHERE generation_id=?`, id).Scan(&issues); err != nil {
		return Generation{}, err
	}
	if err := store.Audit(ctx, tx, "", "inventory_published", "inventory", id, map[string]any{"source_id": source.ID, "generation": current.Number, "files": current.FileCount, "issues": issues}, now); err != nil {
		return Generation{}, err
	}
	if _, err := tx.ExecContext(ctx, dropPublicationPlan); err != nil {
		return Generation{}, err
	}
	job, err := s.queue.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return Generation{}, err
	}
	if final {
		if _, err := s.queue.ProgressTx(ctx, tx, lease, job.Counts, true); err != nil {
			return Generation{}, err
		}
		if _, err := s.queue.CompleteTx(ctx, tx, lease); err != nil {
			return Generation{}, err
		}
	}
	return current, tx.Commit()
}

func (s *Inventory) verifyManifest(ctx context.Context, generation Generation, source library.Source, root media.SourceRoot) error {
	summary, err := Walk(ctx, root, s.policy, func(file File) error {
		expected, err := readFile(s.db.QueryRowContext(ctx, fileSelect+` WHERE generation_id=? AND relative_path=?`, generation.ID, file.RelativePath))
		if errors.Is(err, sql.ErrNoRows) {
			return &PathError{file.RelativePath, ErrChanged}
		}
		if err != nil {
			return err
		}
		if expected != file {
			return &PathError{file.RelativePath, ErrChanged}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if summary.Files != generation.FileCount || summary.Bytes != generation.SizeBytes {
		return ErrChanged
	}
	if summary.Files < source.MinimumFiles {
		return ErrMinimumFiles
	}
	return nil
}

func (s *Inventory) publishPeople(ctx context.Context, tx *sql.Tx, lease jobs.Lease, source library.Source) error {
	after := ""
	for {
		type person struct {
			path, name string
			id         sql.NullString
		}
		rows, err := tx.QueryContext(ctx, `SELECT path,name,person_id FROM gg_publish_people WHERE path>? ORDER BY path LIMIT ?`, after, s.policy.BatchSize)
		if err != nil {
			return err
		}
		batch := make([]person, 0, s.policy.BatchSize)
		for rows.Next() {
			var p person
			if err := rows.Scan(&p.path, &p.name, &p.id); err != nil {
				rows.Close()
				return err
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
		for _, p := range batch {
			now := s.clock()
			version := int64(1)
			var display sql.NullString
			changed := true
			if !p.id.Valid {
				p.id = sql.NullString{String: store.NewID(), Valid: true}
				if _, err := tx.ExecContext(ctx, `INSERT INTO people(id,source_id,relative_path,source_name,created_at,updated_at) VALUES(?,?,?,?,?,?)`, p.id, source.ID, p.path, p.name, now.UnixMilli(), now.UnixMilli()); err != nil {
					return err
				}
			} else {
				var oldPath, oldName string
				if err := tx.QueryRowContext(ctx, `SELECT relative_path,source_name,display_name,version FROM people WHERE id=? AND source_id=?`, p.id, source.ID).Scan(&oldPath, &oldName, &display, &version); err != nil {
					return err
				}
				changed = oldPath != p.path || oldName != p.name
				if changed {
					version++
					if _, err := tx.ExecContext(ctx, `UPDATE people SET relative_path=?,source_name=?,version=?,updated_at=max(updated_at,?) WHERE id=?`, p.path, p.name, version, now.UnixMilli(), p.id); err != nil {
						return err
					}
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE gg_publish_people SET person_id=? WHERE path=?`, p.id, p.path); err != nil {
				return err
			}
			if changed {
				if err := store.Change(ctx, tx, "system", "person", p.id.String, "upsert", version, map[string]any{"id": p.id.String, "source_id": source.ID, "source_name": p.name, "display_name": nullableString(display), "version": version}, now); err != nil {
					return err
				}
			}
		}
		after = batch[len(batch)-1].path
		if err := s.queue.HeartbeatTx(ctx, tx, lease); err != nil {
			return err
		}
	}
	return nil
}

func (s *Inventory) publishAlbums(ctx context.Context, tx *sql.Tx, lease jobs.Lease, source library.Source, scopesRevoked bool) error {
	after := ""
	for {
		type album struct {
			path, name, personPath string
			id                     sql.NullString
		}
		rows, err := tx.QueryContext(ctx, `SELECT path,name,person_path,album_id FROM gg_publish_albums WHERE path>? ORDER BY path LIMIT ?`, after, s.policy.BatchSize)
		if err != nil {
			return err
		}
		batch := make([]album, 0, s.policy.BatchSize)
		for rows.Next() {
			var a album
			if err := rows.Scan(&a.path, &a.name, &a.personPath, &a.id); err != nil {
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
			var mapped sql.NullString
			if a.personPath != "" {
				if err := tx.QueryRowContext(ctx, `SELECT person_id FROM gg_publish_people WHERE path=?`, a.personPath).Scan(&mapped); err != nil {
					return err
				}
			}
			if !a.id.Valid {
				a.id = sql.NullString{String: store.NewID(), Valid: true}
				if _, err := tx.ExecContext(ctx, `INSERT INTO albums(id,library_id,source_id,person_id,mapped_person_id,source_name,relative_path,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, a.id, source.LibraryID, source.ID, mapped, mapped, a.name, a.path, now.UnixMilli(), now.UnixMilli()); err != nil {
					return err
				}
				if err := dirtyAlbum(ctx, tx, a.id.String, true); err != nil {
					return err
				}
			} else {
				var previousLibrary, previousPath, previousName, mode string
				var previousPerson, previousMapped, override sql.NullString
				if err := tx.QueryRowContext(ctx, `SELECT library_id,relative_path,source_name,person_id,mapped_person_id,person_mode,person_override_id FROM albums WHERE id=? AND source_id=?`, a.id, source.ID).Scan(&previousLibrary, &previousPath, &previousName, &previousPerson, &previousMapped, &mode, &override); err != nil {
					return err
				}
				projected, err := access.ProjectAlbumCreatorTx(ctx, tx, sql.NullString{String: source.ID, Valid: true}, mode, mapped, override)
				if err != nil {
					return err
				}
				if previousPerson != projected && !scopesRevoked {
					if err := access.RevokeInventoryScopesTx(ctx, tx, source.ID, now); err != nil {
						return err
					}
					scopesRevoked = true
				}
				if previousLibrary != source.LibraryID || previousPath != a.path || previousName != a.name || previousPerson != projected || previousMapped != mapped {
					if _, err := tx.ExecContext(ctx, `UPDATE albums SET library_id=?,relative_path=?,source_name=?,person_id=?,mapped_person_id=? WHERE id=?`, source.LibraryID, a.path, a.name, projected, mapped, a.id); err != nil {
						return err
					}
					if err := dirtyAlbum(ctx, tx, a.id.String, false); err != nil {
						return err
					}
				}
			}
			// Planning provenance remains indexed, never a manual association. A
			// future whole-folder move must not rename the selected override person.
			if _, err := tx.ExecContext(ctx, `UPDATE gg_publish_albums SET album_id=?,person_id=? WHERE path=?`, a.id, mapped, a.path); err != nil {
				return err
			}
		}
		after = batch[len(batch)-1].path
		if err := s.queue.HeartbeatTx(ctx, tx, lease); err != nil {
			return err
		}
	}
	return nil
}

// Retained empty/missing albums still belong to the configured source library.
// Rehome them only after health validation and only inside the selected scope;
// otherwise a valid favorite could remain hidden forever after a library edit.
func (s *Inventory) rehomeRetainedAlbums(ctx context.Context, tx *sql.Tx, lease jobs.Lease, generation Generation, source library.Source) error {
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM albums WHERE source_id=? AND library_id!=? AND id>? AND (?='.' OR relative_path=? OR substr(relative_path,1,length(?)+1)=?||'/') ORDER BY id LIMIT ?`, source.ID, source.LibraryID, after, generation.scope, generation.scope, generation.scope, generation.scope, s.policy.BatchSize)
		if err != nil {
			return err
		}
		ids := make([]string, 0, s.policy.BatchSize)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE albums SET library_id=? WHERE id=?`, source.LibraryID, id); err != nil {
				return err
			}
			if err := dirtyAlbum(ctx, tx, id, false); err != nil {
				return err
			}
		}
		if err := s.workProgress(ctx, tx, lease, int64(len(ids)), 0, 0); err != nil {
			return err
		}
		if err := s.queue.HeartbeatTx(ctx, tx, lease); err != nil {
			return err
		}
		after = ids[len(ids)-1]
	}
	return nil
}

func (s *Inventory) workProgress(ctx context.Context, tx *sql.Tx, lease jobs.Lease, succeeded, failed, skipped int64) error {
	job, err := s.queue.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return err
	}
	added := succeeded + failed + skipped
	counts := job.Counts
	if added < 0 || added > math.MaxInt64-counts.Total || added > math.MaxInt64-counts.Done || succeeded > math.MaxInt64-counts.Succeeded || failed > math.MaxInt64-counts.Failed || skipped > math.MaxInt64-counts.Skipped {
		return ErrLimit
	}
	counts.Total += added
	counts.Done += added
	counts.Succeeded += succeeded
	counts.Failed += failed
	counts.Skipped += skipped
	_, err = s.queue.ProgressTx(ctx, tx, lease, counts, false)
	return err
}

func nullableString(value sql.NullString) any {
	if value.Valid {
		return value.String
	}
	return nil
}
