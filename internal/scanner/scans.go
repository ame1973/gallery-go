package scanner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

var (
	ErrScanSelection = errors.New("select an active source or source-backed album and a supported scan mode")
	ErrNoSources     = errors.New("no active sources are selected")
)

// Only these explicitly nullable pointer fields accept JSON null. Omission and
// null mean the same selection; empty strings are never interpreted as all sources.
type ScanRequest struct {
	SourceID *string `json:"source_id" nullable:"true"`
	AlbumID  *string `json:"album_id" nullable:"true"`
	Mode     string  `json:"mode"`
}

type scanPayload struct {
	Version int    `json:"version"`
	Mode    string `json:"mode"`
}

type scanTarget struct {
	sourceID, scope string
	config          int64
	album           sql.NullString
}

func (request ScanRequest) validate() error {
	if request.Mode != "full" && request.Mode != "incremental" {
		return ErrScanSelection
	}
	for _, id := range []*string{request.SourceID, request.AlbumID} {
		if id != nil && (*id == "" || len(*id) > 64 || !utf8.ValidString(*id)) {
			return ErrScanSelection
		}
	}
	return nil
}
func (request ScanRequest) selection() string {
	if request.AlbumID != nil {
		return "album"
	}
	if request.SourceID != nil {
		return "source"
	}
	return "all"
}

type Scans struct {
	db    *sql.DB
	auth  *auth.Service
	queue *jobs.Queue
	now   func() time.Time
}

func NewScans(db *sql.DB, authentication *auth.Service, queue *jobs.Queue) *Scans {
	return &Scans{db: db, auth: authentication, queue: queue, now: time.Now}
}
func (s *Scans) clock() time.Time { return s.now().UTC().Truncate(time.Millisecond) }

// Enqueue freezes canonical target/config epochs and dependency keys with the job,
// receipt, audit and outbox. It performs no filesystem I/O and holds no full target
// set in Go or private JSON; bounded SQL pages are read twice in the same snapshot.
func (s *Scans) Enqueue(ctx context.Context, current auth.Session, request ScanRequest, key string) (jobs.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return jobs.Job{}, err
	}
	defer tx.Rollback()
	actor, err := s.auth.RequireAdmin(ctx, tx, current)
	if err != nil {
		return jobs.Job{}, err
	}
	if err := request.validate(); err != nil {
		return jobs.Job{}, err
	}
	metadata, _ := json.Marshal(request)
	command := store.Command{Actor: actor.User.ID, Key: key, Operation: "scan.enqueue", Metadata: string(metadata)}
	var job jobs.Job
	if replayed, err := command.Replay(ctx, tx, s.clock(), &job); replayed || err != nil {
		return job, err
	}
	job, created, count, err := s.enqueueTx(ctx, tx, request, actor.User.ID)
	if err != nil {
		return jobs.Job{}, err
	}
	if _, err := s.auth.RequireAdmin(ctx, tx, current); err != nil {
		return jobs.Job{}, err
	}
	event := "scan_enqueued"
	if !created {
		event = "scan_coalesced"
	}
	now := s.clock()
	if err := store.Audit(ctx, tx, actor.User.ID, event, "job", job.ID, map[string]any{"mode": request.Mode, "selection": request.selection(), "target_count": count}, now); err != nil {
		return jobs.Job{}, err
	}
	if err := command.Save(ctx, tx, job, now); err != nil {
		return jobs.Job{}, err
	}
	return job, tx.Commit()
}

// enqueueTx is shared only by authorized administrative commands and the internal
// scheduler. It freezes the same effective intent, never creates actor credentials,
// and never expands an existing/coalesced job's target set.
func (s *Scans) enqueueTx(ctx context.Context, tx *sql.Tx, request ScanRequest, actor string) (jobs.Job, bool, int64, error) {
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	if err := encoder.Encode([]any{"scan", 1, request.Mode, request.selection(), request.AlbumID}); err != nil {
		return jobs.Job{}, false, 0, err
	}
	var count int64
	after := ""
	for {
		targets, err := selectionBatch(ctx, tx, request, after)
		if err != nil {
			return jobs.Job{}, false, 0, err
		}
		if len(targets) == 0 {
			break
		}
		for _, target := range targets {
			if err := encoder.Encode([]any{target.sourceID, target.config, target.scope, nullableString(target.album)}); err != nil {
				return jobs.Job{}, false, 0, err
			}
			count++
		}
		after = targets[len(targets)-1].sourceID
	}
	if count == 0 {
		if request.selection() == "all" {
			return jobs.Job{}, false, 0, ErrNoSources
		}
		return jobs.Job{}, false, 0, sql.ErrNoRows
	}
	primary := "scan:all"
	if request.AlbumID != nil {
		primary = "album:" + *request.AlbumID
	} else if request.SourceID != nil {
		primary = "source:" + *request.SourceID
	}
	payload, _ := json.Marshal(scanPayload{Version: 1, Mode: request.Mode})
	job, created, err := s.queue.EnqueueTx(ctx, tx, jobs.Input{Kind: "scan", ResourceKey: primary, Dedupe: "scan:" + hex.EncodeToString(hash.Sum(nil)), Payload: payload, ActorID: actor})
	if err != nil {
		return jobs.Job{}, false, 0, err
	}
	if created {
		after = ""
		for {
			targets, err := selectionBatch(ctx, tx, request, after)
			if err != nil {
				return jobs.Job{}, false, 0, err
			}
			if len(targets) == 0 {
				break
			}
			for _, target := range targets {
				if _, err := tx.ExecContext(ctx, `INSERT INTO scan_targets(job_id,source_id,source_config_version,scope_path,album_id) VALUES(?,?,?,?,?)`, job.ID, target.sourceID, target.config, target.scope, target.album); err != nil {
					return jobs.Job{}, false, 0, err
				}
				if err := s.queue.BindResourceTx(ctx, tx, job, "source:"+target.sourceID); err != nil {
					return jobs.Job{}, false, 0, err
				}
			}
			after = targets[len(targets)-1].sourceID
		}
	}
	return job, created, count, nil
}

func selectionBatch(ctx context.Context, tx *sql.Tx, request ScanRequest, after string) ([]scanTarget, error) {
	const sourceColumns = `SELECT id,config_version,'.',NULL,is_active FROM sources `
	query := sourceColumns + `WHERE is_active=1 AND id>? ORDER BY id LIMIT 100`
	args := []any{after}
	if request.AlbumID != nil {
		query = `SELECT s.id,s.config_version,a.relative_path,a.id,s.is_active FROM albums a LEFT JOIN sources s ON s.id=a.source_id WHERE a.id=? AND (s.id>? OR s.id IS NULL) ORDER BY s.id LIMIT 100`
		args = []any{*request.AlbumID, after}
	} else if request.SourceID != nil {
		query = sourceColumns + `WHERE id=? AND id>? ORDER BY id LIMIT 100`
		args = []any{*request.SourceID, after}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make([]scanTarget, 0, 100)
	for rows.Next() {
		var source, path, album sql.NullString
		var config sql.NullInt64
		var active sql.NullBool
		if err := rows.Scan(&source, &config, &path, &album, &active); err != nil {
			return nil, err
		}
		if !source.Valid || !config.Valid || config.Int64 < 1 || !active.Valid || !active.Bool || !path.Valid || (path.String != "." && !validRelative(path.String)) {
			return nil, ErrScanSelection
		}
		if request.SourceID != nil && source.String != *request.SourceID {
			return nil, ErrScanSelection
		}
		targets = append(targets, scanTarget{sourceID: source.String, config: config.Int64, scope: path.String, album: album})
	}
	return targets, rows.Err()
}
