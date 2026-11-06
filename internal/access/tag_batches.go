package access

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

var ErrTagBatchSelection = errors.New("select 1 to 100 unique visible album IDs and an explicit set of at most 100 existing tag IDs")

type TagBatchRequest struct {
	AlbumIDs []string `json:"album_ids"`
	TagIDs   []string `json:"tag_ids"`
}

type TagBatchTarget struct {
	AlbumID             string   `json:"album_id"`
	AlbumVersion        int64    `json:"album_version"`
	LibraryID           string   `json:"library_id"`
	SourceID            *string  `json:"source_id"`
	SourceConfigVersion *int64   `json:"source_config_version"`
	TagIDs              []string `json:"tag_ids"`
}

// TagBatches freezes accepted intent and processes its metadata-only targets under
// current original-actor authority and live queue leases. Enqueue is not completion.
type TagBatches struct {
	db    *sql.DB
	auth  *auth.Service
	queue *jobs.Queue
	now   func() time.Time
}

func NewTagBatches(db *sql.DB, authentication *auth.Service, queue *jobs.Queue) *TagBatches {
	return &TagBatches{db: db, auth: authentication, queue: queue, now: time.Now}
}

func (b *TagBatches) Enqueue(ctx context.Context, current auth.Session, input TagBatchRequest, key string) (jobs.Job, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return jobs.Job{}, err
	}
	defer tx.Rollback()
	actor, err := b.auth.RequireAdmin(ctx, tx, current)
	if err != nil {
		return jobs.Job{}, err
	}
	albums, _, err := normalizedGroups(input.AlbumIDs)
	if err != nil || len(albums) == 0 {
		return jobs.Job{}, ErrTagBatchSelection
	}
	tags, tagJSON, err := normalizedGroups(input.TagIDs)
	if err != nil {
		return jobs.Job{}, ErrTagBatchSelection
	}
	input = TagBatchRequest{AlbumIDs: albums, TagIDs: tags}
	now := b.now().UTC().Truncate(time.Millisecond)
	metadata, _ := json.Marshal(input)
	command := store.Command{Actor: actor.User.ID, Key: key, Operation: "batch.tags.enqueue", Metadata: string(metadata)}
	var job jobs.Job
	if found, err := command.Replay(ctx, tx, now, &job); found || err != nil {
		return job, err
	}
	var known int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags WHERE id IN (SELECT value FROM json_each(?))`, tagJSON).Scan(&known); err != nil {
		return jobs.Job{}, err
	}
	if known != len(tags) {
		return jobs.Job{}, ErrUnknownTag
	}
	targets := make([]TagBatchTarget, 0, len(albums))
	for _, id := range albums {
		var target TagBatchTarget
		if err := tx.QueryRowContext(ctx, `SELECT a.id,a.version,a.library_id,a.source_id,s.config_version FROM albums a JOIN libraries l ON l.id=a.library_id LEFT JOIN sources s ON s.id=a.source_id WHERE `+AlbumVisible+` AND a.id=?`, actor.User.ID, id).Scan(&target.AlbumID, &target.AlbumVersion, &target.LibraryID, &target.SourceID, &target.SourceConfigVersion); err != nil {
			return jobs.Job{}, err
		}
		if target.AlbumVersion < 1 || (target.SourceID != nil && (target.SourceConfigVersion == nil || *target.SourceConfigVersion < 1)) {
			return jobs.Job{}, ErrTagBatchSelection
		}
		target.TagIDs = tags
		targets = append(targets, target)
	}
	intent, _ := json.Marshal(struct {
		Version int              `json:"version"`
		Actor   string           `json:"actor"`
		Targets []TagBatchTarget `json:"targets"`
	}{1, actor.User.ID, targets})
	digest := sha256.Sum256(intent)
	identity := "batch_tags:" + hex.EncodeToString(digest[:])
	job, created, err := b.queue.EnqueueTx(ctx, tx, jobs.Input{Kind: "batch_tags", ResourceKey: identity, Dedupe: identity, Payload: json.RawMessage(`{"version":1}`), ActorID: actor.User.ID})
	if err != nil {
		return jobs.Job{}, err
	}
	if created {
		if _, err := tx.ExecContext(ctx, `INSERT INTO album_tag_batches(job_id,actor_user_id,tag_ids_json,created_at) VALUES(?,?,?,?)`, job.ID, actor.User.ID, tagJSON, now.UnixMilli()); err != nil {
			return jobs.Job{}, err
		}
		for _, target := range targets {
			if _, err := tx.ExecContext(ctx, `INSERT INTO album_tag_batch_targets(job_id,album_id,album_version,library_id,source_id,source_config_version) VALUES(?,?,?,?,?,?)`, job.ID, target.AlbumID, target.AlbumVersion, target.LibraryID, target.SourceID, target.SourceConfigVersion); err != nil {
				return jobs.Job{}, err
			}
			if err := b.queue.BindResourceTx(ctx, tx, job, "album:"+target.AlbumID); err != nil {
				return jobs.Job{}, err
			}
			if target.SourceID != nil {
				if err := b.queue.BindResourceTx(ctx, tx, job, "source:"+*target.SourceID); err != nil {
					return jobs.Job{}, err
				}
			}
		}
	}
	if _, err := b.auth.RequireAdmin(ctx, tx, current); err != nil {
		return jobs.Job{}, err
	}
	event := "tag_batch_enqueued"
	if !created {
		event = "tag_batch_coalesced"
	}
	if err := store.Audit(ctx, tx, actor.User.ID, event, "job", job.ID, map[string]any{"target_count": len(targets), "tag_ids": tags}, now); err != nil {
		return jobs.Job{}, err
	}
	if err := command.Save(ctx, tx, job, now); err != nil {
		return jobs.Job{}, err
	}
	return job, tx.Commit()
}

func (b *TagBatches) Targets(ctx context.Context, current auth.Session, id, after string, limit int) ([]TagBatchTarget, error) {
	if !pageLimit(limit) {
		return nil, ErrTagBatchSelection
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := b.auth.RequireAdmin(ctx, tx, current); err != nil {
		return nil, err
	}
	var data string
	if err := tx.QueryRowContext(ctx, `SELECT tag_ids_json FROM album_tag_batches WHERE job_id=?`, id).Scan(&data); err != nil {
		return nil, err
	}
	var tags []string
	if err := json.Unmarshal([]byte(data), &tags); err != nil {
		return nil, err
	}
	if _, _, err := normalizedGroups(tags); err != nil {
		return nil, errors.New("invalid stored batch tag set")
	}
	rows, err := tx.QueryContext(ctx, `SELECT album_id,album_version,library_id,source_id,source_config_version FROM album_tag_batch_targets WHERE job_id=? AND album_id>? ORDER BY album_id LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := []TagBatchTarget{}
	for rows.Next() {
		var target TagBatchTarget
		if err := rows.Scan(&target.AlbumID, &target.AlbumVersion, &target.LibraryID, &target.SourceID, &target.SourceConfigVersion); err != nil {
			return nil, err
		}
		target.TagIDs = tags
		targets = append(targets, target)
	}
	return targets, rows.Err()
}
