package scanner

import (
	"context"
	"database/sql"
	"strings"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/store"
)

// Connection-local planning tables freeze all rename candidates before any
// indexed path mutates. Otherwise processing one duplicate can make the next
// duplicate appear unique. SQLite owns the indexed working set, not the Go heap.
const createPublicationPlan = `
CREATE TEMP TABLE gg_publish_paths(path TEXT PRIMARY KEY,entry_id TEXT UNIQUE,album_path TEXT NOT NULL,album_name TEXT NOT NULL,person_path TEXT NOT NULL,person_name TEXT NOT NULL,is_media INTEGER NOT NULL,selected INTEGER NOT NULL);
CREATE INDEX gg_publish_path_album ON gg_publish_paths(album_path,entry_id);
CREATE TEMP TABLE gg_publish_hashes(sha256 TEXT NOT NULL,size_bytes INTEGER NOT NULL,new_count INTEGER NOT NULL,old_count INTEGER NOT NULL DEFAULT 0,entry_id TEXT,PRIMARY KEY(sha256,size_bytes)) WITHOUT ROWID;
CREATE TEMP TABLE gg_publish_albums(path TEXT PRIMARY KEY,name TEXT NOT NULL,person_path TEXT NOT NULL,person_name TEXT NOT NULL,album_id TEXT UNIQUE,person_id TEXT);
CREATE TEMP TABLE gg_publish_people(path TEXT PRIMARY KEY,name TEXT NOT NULL,person_id TEXT UNIQUE);
CREATE TEMP TABLE gg_publish_album_candidates(path TEXT NOT NULL,id TEXT NOT NULL,PRIMARY KEY(path,id)) WITHOUT ROWID;
CREATE TEMP TABLE gg_publish_person_candidates(path TEXT NOT NULL,id TEXT NOT NULL,PRIMARY KEY(path,id)) WITHOUT ROWID;
CREATE TEMP TABLE gg_publish_dirty_albums(id TEXT PRIMARY KEY,is_new INTEGER NOT NULL DEFAULT 0);
`
const dropPublicationPlan = `DROP TABLE gg_publish_dirty_albums; DROP TABLE gg_publish_person_candidates; DROP TABLE gg_publish_album_candidates; DROP TABLE gg_publish_people; DROP TABLE gg_publish_albums; DROP TABLE gg_publish_hashes; DROP TABLE gg_publish_paths;`

func inScope(scope, relative string) bool {
	return scope == "." || relative == scope || strings.HasPrefix(relative, scope+"/")
}

func (s *Inventory) publicationPlan(ctx context.Context, tx *sql.Tx, lease jobs.Lease, generation Generation, source library.Source) error {
	if _, err := tx.ExecContext(ctx, createPublicationPlan); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gg_publish_hashes(sha256,size_bytes,new_count)
	 SELECT f.sha256,f.size_bytes,COUNT(*) FROM inventory_files f WHERE f.generation_id=?
	 AND NOT EXISTS(SELECT 1 FROM source_entries e WHERE e.source_id=? AND e.relative_path=f.relative_path)
	 GROUP BY f.sha256,f.size_bytes`, generation.ID, source.ID); err != nil {
		return err
	}
	if err := s.queue.HeartbeatTx(ctx, tx, lease); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gg_publish_hashes AS h SET
	 old_count=(SELECT COUNT(*) FROM source_entries e WHERE e.source_id=? AND e.sha256=h.sha256 AND e.size_bytes=h.size_bytes AND NOT EXISTS(SELECT 1 FROM inventory_files f WHERE f.generation_id=? AND f.relative_path=e.relative_path)),
	 entry_id=(SELECT MIN(e.id) FROM source_entries e WHERE e.source_id=? AND e.sha256=h.sha256 AND e.size_bytes=h.size_bytes AND NOT EXISTS(SELECT 1 FROM inventory_files f WHERE f.generation_id=? AND f.relative_path=e.relative_path))`, source.ID, generation.ID, source.ID, generation.ID); err != nil {
		return err
	}
	if err := s.queue.HeartbeatTx(ctx, tx, lease); err != nil {
		return err
	}
	after := ""
	for {
		type planned struct {
			path, kind, albumPath, albumName, personPath, personName string
			entry                                                    sql.NullString
			selected, ambiguous, outside                             bool
		}
		rows, err := tx.QueryContext(ctx, `SELECT f.relative_path,md.kind,e.id,COALESCE(h.new_count,0),COALESCE(h.old_count,0),h.entry_id,renamed.relative_path
		 FROM inventory_files f JOIN inventory_media md ON md.generation_id=f.generation_id AND md.relative_path=f.relative_path
		 LEFT JOIN source_entries e ON e.source_id=? AND e.relative_path=f.relative_path
		 LEFT JOIN gg_publish_hashes h ON h.sha256=f.sha256 AND h.size_bytes=f.size_bytes
		 LEFT JOIN source_entries renamed ON renamed.id=h.entry_id
		 WHERE f.generation_id=? AND f.relative_path>? ORDER BY f.relative_path LIMIT ?`, source.ID, generation.ID, after, s.policy.BatchSize)
		if err != nil {
			return err
		}
		batch := make([]planned, 0, s.policy.BatchSize)
		for rows.Next() {
			var p planned
			var added, missing int64
			var candidate, oldPath sql.NullString
			if err := rows.Scan(&p.path, &p.kind, &p.entry, &added, &missing, &candidate, &oldPath); err != nil {
				rows.Close()
				return err
			}
			p.selected = inScope(generation.scope, p.path)
			if !p.entry.Valid {
				if added == 1 && missing == 1 && candidate.Valid {
					p.entry = candidate
					if p.selected != inScope(generation.scope, oldPath.String) {
						p.selected = false
						p.outside = true
					}
				} else {
					p.ambiguous = p.selected && missing > 0
				}
			}
			location, err := source.Mapping.Locate(p.path, source.RootName)
			if err != nil {
				rows.Close()
				return err
			}
			p.albumPath, p.albumName, p.personPath, p.personName = location.AlbumPath, location.AlbumName, location.PersonPath, location.PersonName
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
			isMedia := p.kind != ""
			if !isMedia && p.entry.Valid {
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM media WHERE source_entry_id=?)`, p.entry).Scan(&isMedia); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO gg_publish_paths VALUES(?,?,?,?,?,?,?,?)`, p.path, p.entry, p.albumPath, p.albumName, p.personPath, p.personName, isMedia, p.selected); err != nil {
				return err
			}
			code := ""
			if p.ambiguous {
				code = "AMBIGUOUS_RENAME"
			}
			if p.outside {
				code = "TARGET_RENAME_OUTSIDE_SCOPE"
			}
			if code != "" {
				if err := s.publicationIssue(ctx, tx, generation.ID, p.path, code); err != nil {
					return err
				}
			}
		}
		after = batch[len(batch)-1].path
		if err := s.queue.HeartbeatTx(ctx, tx, lease); err != nil {
			return err
		}
	}
	for _, step := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO gg_publish_albums(path,name,person_path,person_name) SELECT album_path,album_name,person_path,person_name FROM gg_publish_paths WHERE selected=1 AND is_media=1 GROUP BY album_path`, nil},
		{`UPDATE gg_publish_albums AS p SET album_id=(SELECT id FROM albums WHERE source_id=? AND relative_path=p.path)`, []any{source.ID}},
		{`INSERT INTO gg_publish_album_candidates(path,id)
		 SELECT p.album_path,a.id FROM gg_publish_paths p JOIN media m ON m.source_entry_id=p.entry_id JOIN albums a ON a.id=m.album_id
		 JOIN gg_publish_albums dest ON dest.path=p.album_path AND dest.album_id IS NULL
		 WHERE p.selected=1 AND a.source_id=? AND NOT EXISTS(SELECT 1 FROM gg_publish_albums claimed WHERE claimed.album_id=a.id)
		 AND NOT EXISTS(SELECT 1 FROM inventory_files f WHERE f.generation_id=? AND (a.relative_path='.' OR f.relative_path=a.relative_path OR substr(f.relative_path,1,length(a.relative_path)+1)=a.relative_path||'/'))
		 AND NOT EXISTS(SELECT 1 FROM gg_publish_paths mixed JOIN media other ON other.source_entry_id=mixed.entry_id WHERE mixed.album_path=p.album_path AND other.album_id!=a.id)
		 GROUP BY p.album_path,a.id HAVING COUNT(DISTINCT m.id)=(SELECT COUNT(*) FROM media owned WHERE owned.album_id=a.id)`, []any{source.ID, generation.ID}},
		{`UPDATE gg_publish_albums AS p SET album_id=(SELECT MIN(id) FROM gg_publish_album_candidates c WHERE c.path=p.path HAVING COUNT(*)=1) WHERE album_id IS NULL`, nil},
		{`INSERT INTO gg_publish_people(path,name) SELECT person_path,person_name FROM gg_publish_albums WHERE person_path!='' GROUP BY person_path`, nil},
		{`UPDATE gg_publish_people AS p SET person_id=(SELECT id FROM people WHERE source_id=? AND relative_path=p.path)`, []any{source.ID}},
		{`INSERT INTO gg_publish_person_candidates(path,id)
		 SELECT planned.person_path,old.id FROM gg_publish_albums planned JOIN albums a ON a.id=planned.album_id JOIN people old ON old.id=a.person_id
		 JOIN gg_publish_people dest ON dest.path=planned.person_path AND dest.person_id IS NULL
		 WHERE old.source_id=? AND NOT EXISTS(SELECT 1 FROM gg_publish_people claimed WHERE claimed.person_id=old.id)
		 AND NOT EXISTS(SELECT 1 FROM inventory_files f WHERE f.generation_id=? AND (f.relative_path=old.relative_path OR substr(f.relative_path,1,length(old.relative_path)+1)=old.relative_path||'/'))
		 AND NOT EXISTS(SELECT 1 FROM gg_publish_albums mixed JOIN albums other ON other.id=mixed.album_id WHERE mixed.person_path=planned.person_path AND other.person_id IS NOT NULL AND other.person_id!=old.id)
		 GROUP BY planned.person_path,old.id HAVING COUNT(DISTINCT a.id)=(SELECT COUNT(*) FROM albums owned WHERE owned.person_id=old.id)`, []any{source.ID, generation.ID}},
		{`UPDATE gg_publish_people AS p SET person_id=(SELECT MIN(id) FROM gg_publish_person_candidates c WHERE c.path=p.path HAVING COUNT(*)=1) WHERE person_id IS NULL`, nil},
	} {
		if _, err := tx.ExecContext(ctx, step.query, step.args...); err != nil {
			return err
		}
		if err := s.queue.HeartbeatTx(ctx, tx, lease); err != nil {
			return err
		}
	}
	// Mixed folder moves are diagnosed, not interpreted as ownership of one of
	// the old albums. All old objects/interactions remain retained separately.
	var afterAlbum string
	for {
		rows, err := tx.QueryContext(ctx, `SELECT p.album_path FROM gg_publish_paths p JOIN media m ON m.source_entry_id=p.entry_id JOIN gg_publish_albums a ON a.path=p.album_path WHERE p.selected=1 AND a.album_id IS NULL AND p.album_path>? GROUP BY p.album_path HAVING COUNT(DISTINCT m.album_id)>1 ORDER BY p.album_path LIMIT ?`, afterAlbum, s.policy.BatchSize)
		if err != nil {
			return err
		}
		paths := make([]string, 0, s.policy.BatchSize)
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return err
			}
			paths = append(paths, path)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			break
		}
		for _, path := range paths {
			if err := s.publicationIssue(ctx, tx, generation.ID, path, "AMBIGUOUS_ALBUM_RENAME"); err != nil {
				return err
			}
		}
		afterAlbum = paths[len(paths)-1]
	}
	return nil
}

func (s *Inventory) publicationIssue(ctx context.Context, tx *sql.Tx, id, path, code string) error {
	if path != "." && !validRelative(path) {
		return ErrUnsafeEntry
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO inventory_issues(id,generation_id,relative_path,error_code,created_at) VALUES(?,?,?,?,?) ON CONFLICT(generation_id,relative_path,error_code) DO NOTHING`, store.NewID(), id, path, code, s.clock().UnixMilli())
	return err
}

func dirtyAlbum(ctx context.Context, tx *sql.Tx, id string, isNew bool) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO gg_publish_dirty_albums(id,is_new) VALUES(?,?) ON CONFLICT(id) DO NOTHING`, id, isNew)
	return err
}
