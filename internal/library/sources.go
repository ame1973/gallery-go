package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

type Sources struct {
	db    *sql.DB
	auth  *auth.Service
	queue *jobs.Queue
	roots map[string]media.SourceRoot
	now   func() time.Time
}

func NewSources(db *sql.DB, authentication *auth.Service, queue *jobs.Queue, roots map[string]media.SourceRoot) *Sources {
	return &Sources{db: db, auth: authentication, queue: queue, roots: maps.Clone(roots), now: time.Now}
}

func (s *Sources) clock() time.Time { return s.now().UTC().Truncate(time.Millisecond) }

func (s *Sources) adminTx(ctx context.Context, current auth.Session) (*sql.Tx, auth.User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, auth.User{}, err
	}
	actor, err := s.auth.RequireAdmin(ctx, tx, current)
	if err != nil {
		tx.Rollback()
		return nil, auth.User{}, err
	}
	return tx, actor.User, nil
}

func validRoot(root media.SourceRoot) bool {
	return filepath.IsAbs(root.Path) && root.Marker != "." && fs.ValidPath(root.Marker) && !strings.ContainsAny(root.Marker, "\\\x00")
}

func (s *Sources) root(name string) (media.SourceRoot, error) {
	root, ok := s.roots[name]
	if !ok || !ValidRootName(name) || !validRoot(root) {
		return media.SourceRoot{}, ErrUnknownRoot
	}
	return root, nil
}

func (s *Sources) read(row interface{ Scan(...any) error }) (Source, error) {
	source, err := scanSource(row)
	if err != nil {
		return Source{}, err
	}
	root, err := s.root(source.RootName)
	source.Configured = err == nil && source.rootPath == root.Path && source.rootSignature == root.Signature()
	return source, nil
}

func (s *Sources) Roots(ctx context.Context, current auth.Session, after string, limit int) ([]Root, error) {
	if limit < 1 || limit > 101 {
		return nil, ErrSourceSettings
	}
	tx, _, err := s.adminTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// This is the bounded YAML registry already held in memory, not a media query.
	names := slices.Sorted(maps.Keys(s.roots))
	items := make([]Root, 0)
	for _, name := range names {
		if name > after {
			items = append(items, Root{Name: name})
			if len(items) == limit {
				break
			}
		}
	}
	return items, nil
}

func (s *Sources) List(ctx context.Context, current auth.Session, after string, limit int) ([]Source, error) {
	if limit < 1 || limit > 101 {
		return nil, ErrSourceSettings
	}
	tx, _, err := s.adminTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, sourceSelect+` WHERE id>? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Source, 0)
	for rows.Next() {
		source, err := s.read(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, source)
	}
	return items, rows.Err()
}

func (s *Sources) Get(ctx context.Context, current auth.Session, id string) (Source, error) {
	tx, _, err := s.adminTx(ctx, current)
	if err != nil {
		return Source{}, err
	}
	defer tx.Rollback()
	return s.read(tx.QueryRowContext(ctx, sourceSelect+` WHERE id=?`, id))
}

func knownLibrary(ctx context.Context, tx *sql.Tx, id string) error {
	var found string
	return tx.QueryRowContext(ctx, `SELECT id FROM libraries WHERE id=?`, id).Scan(&found)
}

func rootUnused(ctx context.Context, tx *sql.Tx, name, root, except string) error {
	var busy bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sources WHERE (name=? OR root_path=?) AND id!=?)`, name, root, except).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return ErrRootInUse
	}
	return nil
}

func (s *Sources) Create(ctx context.Context, current auth.Session, input SourceSettings, key string) (Source, error) {
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return Source{}, err
	}
	defer tx.Rollback()
	if err := input.Validate(); err != nil {
		return Source{}, err
	}
	now := s.clock()
	data, _ := json.Marshal(input)
	command := store.Command{Actor: actor.ID, Key: key, Operation: "source.create", Metadata: string(data)}
	var source Source
	if found, err := command.Replay(ctx, tx, now, &source); found || err != nil {
		return source, err
	}
	root, err := s.root(input.RootName)
	if err != nil {
		return Source{}, err
	}
	if err := knownLibrary(ctx, tx, input.LibraryID); err != nil {
		return Source{}, err
	}
	if err := rootUnused(ctx, tx, input.RootName, root.Path, ""); err != nil {
		return Source{}, err
	}
	source = Source{ID: store.NewID(), SourceSettings: input, Configured: true, State: "unknown", Version: 1, ConfigVersion: 1, CreatedAt: now, UpdatedAt: now, rootPath: root.Path, rootSignature: root.Signature()}
	mapping, _ := json.Marshal(input.Mapping)
	if _, err := tx.ExecContext(ctx, `INSERT INTO sources(id,library_id,name,root_path,root_config_sha256,mapping_json,is_active,minimum_files,poll_interval_seconds,full_scan_interval_seconds,state,version,config_version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,'unknown',1,1,?,?)`, source.ID, source.LibraryID, source.RootName, source.rootPath, source.rootSignature, string(mapping), source.Active, source.MinimumFiles, source.PollIntervalSeconds, source.FullScanIntervalSeconds, now.UnixMilli(), now.UnixMilli()); err != nil {
		return Source{}, err
	}
	if err := store.Change(ctx, tx, "system", "source", source.ID, "upsert", source.Version, source, now); err != nil {
		return Source{}, err
	}
	if err := store.Audit(ctx, tx, actor.ID, "source_created", "source", source.ID, map[string]any{"root_name": source.RootName, "library_id": source.LibraryID, "active": source.Active}, now); err != nil {
		return Source{}, err
	}
	if err := command.Save(ctx, tx, source, now); err != nil {
		return Source{}, err
	}
	return source, tx.Commit()
}

func (s *Sources) Update(ctx context.Context, current auth.Session, id string, version int64, patch SourcePatch) (Source, error) {
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return Source{}, err
	}
	defer tx.Rollback()
	source, err := s.read(tx.QueryRowContext(ctx, sourceSelect+` WHERE id=?`, id))
	if err != nil {
		return Source{}, err
	}
	if source.Version != version {
		return Source{}, auth.ErrConflict
	}
	before := source
	if err := patch.apply(&source.SourceSettings); err != nil {
		return Source{}, err
	}
	root, err := s.root(source.RootName)
	if err != nil {
		return Source{}, err
	}
	if err := knownLibrary(ctx, tx, source.LibraryID); err != nil {
		return Source{}, err
	}
	if err := rootUnused(ctx, tx, source.RootName, root.Path, id); err != nil {
		return Source{}, err
	}
	if before.SourceSettings == source.SourceSettings && before.rootPath == root.Path && before.rootSignature == root.Signature() {
		return source, nil
	}
	now := s.clock()
	if err := access.RevokeSourceScopesTx(ctx, tx, before.LibraryID, source.LibraryID, now); err != nil {
		return Source{}, err
	}
	source.Version++
	source.ConfigVersion++
	source.UpdatedAt = maxTime(source.UpdatedAt, now)
	source.Configured, source.State, source.rootPath, source.rootSignature = true, "unknown", root.Path, root.Signature()
	source.LastCheckedAt, source.LastHealthyAt, source.LastFullScanAt, source.LastErrorCode = nil, nil, nil, nil
	mapping, _ := json.Marshal(source.Mapping)
	// Do not rehome existing albums from an old snapshot here. A source/library
	// mismatch hides them from both libraries until healthy inventory publication;
	// this avoids granting destination users stale previews from a different root.
	if _, err := tx.ExecContext(ctx, `UPDATE sources SET library_id=?,name=?,root_path=?,root_config_sha256=?,mapping_json=?,is_active=?,minimum_files=?,poll_interval_seconds=?,full_scan_interval_seconds=?,version=?,config_version=?,state='unknown',last_checked_at=NULL,last_healthy_at=NULL,last_full_scan_at=NULL,last_error_code=NULL,next_scan_at=0,next_full_scan_at=0,updated_at=? WHERE id=?`, source.LibraryID, source.RootName, source.rootPath, source.rootSignature, string(mapping), source.Active, source.MinimumFiles, source.PollIntervalSeconds, source.FullScanIntervalSeconds, source.Version, source.ConfigVersion, source.UpdatedAt.UnixMilli(), id); err != nil {
		return Source{}, err
	}
	cancelled, err := s.queue.CancelResourceTx(ctx, tx, "source:"+id, actor.ID)
	if err != nil {
		return Source{}, err
	}
	if err := store.Change(ctx, tx, "system", "source", id, "upsert", source.Version, source, now); err != nil {
		return Source{}, err
	}
	if err := store.Audit(ctx, tx, actor.ID, "source_updated", "source", id, map[string]any{"version": source.Version, "config_version": source.ConfigVersion, "library_id": source.LibraryID, "root_name": source.RootName, "active": source.Active, "cancelled_jobs": cancelled}, now); err != nil {
		return Source{}, err
	}
	return source, tx.Commit()
}

func (s *Sources) DeleteEmpty(ctx context.Context, current auth.Session, id string, version int64) error {
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	source, err := s.read(tx.QueryRowContext(ctx, sourceSelect+` WHERE id=?`, id))
	if err != nil {
		return err
	}
	if source.Version != version {
		return auth.ErrConflict
	}
	var occupied bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM albums WHERE source_id=?) OR EXISTS(SELECT 1 FROM source_entries WHERE source_id=?)`, id, id).Scan(&occupied); err != nil {
		return err
	}
	if occupied {
		return ErrSourceNotEmpty
	}
	now := s.clock()
	if err := access.RevokeSourceScopesTx(ctx, tx, source.LibraryID, source.LibraryID, now); err != nil {
		return err
	}
	if _, err := s.queue.CancelResourceTx(ctx, tx, "source:"+id, actor.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sources WHERE id=?`, id); err != nil {
		return err
	}
	if err := store.Change(ctx, tx, "system", "source", id, "delete", source.Version+1, map[string]string{"id": id}, now); err != nil {
		return err
	}
	if err := store.Audit(ctx, tx, actor.ID, "source_deleted", "source", id, map[string]string{"root_name": source.RootName}, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RequireConfigTx must accompany the caller's RequireLeaseTx in the publishing
// transaction. Registry path/marker changes and source policy epochs fence stale
// processes; no filesystem I/O occurs inside this configuration check.
func (s *Sources) RequireConfigTx(ctx context.Context, tx *sql.Tx, id string, configVersion int64) (Source, media.SourceRoot, error) {
	source, err := s.read(tx.QueryRowContext(ctx, sourceSelect+` WHERE id=?`, id))
	if err != nil {
		return Source{}, media.SourceRoot{}, err
	}
	if !source.Active || !source.Configured || source.ConfigVersion != configVersion {
		return Source{}, media.SourceRoot{}, ErrSourceChanged
	}
	root, err := s.root(source.RootName)
	return source, root, err
}

func maxTime(previous, next time.Time) time.Time {
	if next.After(previous) {
		return next
	}
	return previous
}
