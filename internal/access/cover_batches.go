package access

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

var ErrCoverBatchSelection = errors.New("provide 1 to 100 unique album targets with an explicit cover_media_id; empty string clears the preference")

type CoverBatchSelection struct {
	AlbumID      string  `json:"album_id"`
	CoverMediaID *string `json:"cover_media_id"`
}

type CoverBatchRequest struct {
	Targets []CoverBatchSelection `json:"targets"`
}

// CoverBatchTarget freezes the desired preference and its actual projected media
// epoch. Empty requested ID means clear, not an omitted or null instruction.
type CoverBatchTarget struct {
	AlbumID             string  `json:"album_id"`
	AlbumVersion        int64   `json:"album_version"`
	LibraryID           string  `json:"library_id"`
	SourceID            *string `json:"source_id"`
	SourceConfigVersion *int64  `json:"source_config_version"`
	CoverMediaID        string  `json:"cover_media_id"`
	ProjectedCoverID    *string `json:"projected_cover_media_id"`
	MediaVersion        *int64  `json:"media_version"`
	RevisionID          *string `json:"revision_id"`
}

func normalizeCoverBatch(input CoverBatchRequest) (CoverBatchRequest, error) {
	if len(input.Targets) < 1 || len(input.Targets) > 100 {
		return CoverBatchRequest{}, ErrCoverBatchSelection
	}
	targets := make([]CoverBatchSelection, 0, len(input.Targets))
	ids := make([]string, 0, len(input.Targets))
	for _, target := range input.Targets {
		if target.CoverMediaID == nil {
			return CoverBatchRequest{}, ErrCoverBatchSelection
		}
		requested := *target.CoverMediaID
		if len(requested) > 64 || !utf8.ValidString(requested) || strings.ContainsRune(requested, 0) {
			return CoverBatchRequest{}, ErrCoverBatchSelection
		}
		ids = append(ids, target.AlbumID)
		targets = append(targets, CoverBatchSelection{AlbumID: target.AlbumID, CoverMediaID: &requested})
	}
	if _, _, err := normalizedGroups(ids); err != nil {
		return CoverBatchRequest{}, ErrCoverBatchSelection
	}
	slices.SortFunc(targets, func(a, b CoverBatchSelection) int { return strings.Compare(a.AlbumID, b.AlbumID) })
	return CoverBatchRequest{Targets: targets}, nil
}

func captureCoverTargetTx(ctx context.Context, tx *sql.Tx, actor string, input CoverBatchSelection) (CoverBatchTarget, error) {
	var target CoverBatchTarget
	if err := tx.QueryRowContext(ctx, `SELECT a.id,a.version,a.library_id,a.source_id,s.config_version FROM albums a JOIN libraries l ON l.id=a.library_id LEFT JOIN sources s ON s.id=a.source_id WHERE `+AlbumVisible+` AND a.id=?`, actor, input.AlbumID).Scan(&target.AlbumID, &target.AlbumVersion, &target.LibraryID, &target.SourceID, &target.SourceConfigVersion); err != nil {
		return CoverBatchTarget{}, err
	}
	if target.AlbumVersion < 1 || (target.SourceID != nil && (target.SourceConfigVersion == nil || *target.SourceConfigVersion < 1)) {
		return CoverBatchTarget{}, ErrCoverBatchSelection
	}
	state, err := scanAlbumMetadata(tx.QueryRowContext(ctx, albumMetadataSelect+` WHERE `+AlbumVisible+` AND a.id=?`, actor, actor, target.AlbumID))
	if err != nil {
		return CoverBatchTarget{}, err
	}
	target.CoverMediaID = *input.CoverMediaID
	_, projected, err := projectAlbumCoverTx(ctx, tx, actor, target.AlbumID, target.CoverMediaID, state.rawCover)
	if err != nil {
		return CoverBatchTarget{}, err
	}
	target.ProjectedCoverID = nullableMetadataString(projected)
	if projected.Valid {
		if err := tx.QueryRowContext(ctx, `SELECT m.version,m.current_revision_id FROM media m JOIN albums a ON a.id=m.album_id JOIN libraries l ON l.id=a.library_id WHERE `+MediaVisible+` AND m.album_id=? AND m.id=?`, actor, target.AlbumID, projected.String).Scan(&target.MediaVersion, &target.RevisionID); err != nil {
			return CoverBatchTarget{}, err
		}
		if target.MediaVersion == nil || *target.MediaVersion < 1 {
			return CoverBatchTarget{}, ErrCoverBatchSelection
		}
	}
	return target, nil
}

func (b *AlbumBatches) EnqueueCovers(ctx context.Context, current auth.Session, input CoverBatchRequest, key string) (jobs.Job, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return jobs.Job{}, err
	}
	defer tx.Rollback()
	actor, err := b.auth.RequireAdmin(ctx, tx, current)
	if err != nil {
		return jobs.Job{}, err
	}
	input, err = normalizeCoverBatch(input)
	if err != nil {
		return jobs.Job{}, err
	}
	now := b.now().UTC().Truncate(time.Millisecond)
	body, _ := json.Marshal(input)
	command := store.Command{Actor: actor.User.ID, Key: key, Operation: "batch.covers.enqueue", Metadata: string(body)}
	var job jobs.Job
	if found, err := command.Replay(ctx, tx, now, &job); found || err != nil {
		return job, err
	}
	targets := make([]CoverBatchTarget, 0, len(input.Targets))
	for _, selection := range input.Targets {
		target, err := captureCoverTargetTx(ctx, tx, actor.User.ID, selection)
		if err != nil {
			return jobs.Job{}, err
		}
		targets = append(targets, target)
	}
	intent, _ := json.Marshal(struct {
		Version int                `json:"version"`
		Actor   string             `json:"actor"`
		Targets []CoverBatchTarget `json:"targets"`
	}{1, actor.User.ID, targets})
	digest := sha256.Sum256(intent)
	identity := "batch_covers:" + hex.EncodeToString(digest[:])
	job, created, err := b.queue.EnqueueTx(ctx, tx, jobs.Input{Kind: "batch_covers", ResourceKey: identity, Dedupe: identity, Payload: json.RawMessage(`{"version":1}`), ActorID: actor.User.ID})
	if err != nil {
		return jobs.Job{}, err
	}
	if created {
		if _, err := tx.ExecContext(ctx, `INSERT INTO album_cover_batches(job_id,actor_user_id,created_at) VALUES(?,?,?)`, job.ID, actor.User.ID, now.UnixMilli()); err != nil {
			return jobs.Job{}, err
		}
		for _, target := range targets {
			if _, err := tx.ExecContext(ctx, `INSERT INTO album_cover_batch_targets(job_id,album_id,album_version,library_id,source_id,source_config_version,requested_cover_id,cover_media_id,cover_media_version,cover_revision_id) VALUES(?,?,?,?,?,?,?,?,?,?)`, job.ID, target.AlbumID, target.AlbumVersion, target.LibraryID, target.SourceID, target.SourceConfigVersion, target.CoverMediaID, target.ProjectedCoverID, target.MediaVersion, target.RevisionID); err != nil {
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
			if target.ProjectedCoverID != nil {
				if err := b.queue.BindResourceTx(ctx, tx, job, "media:"+*target.ProjectedCoverID); err != nil {
					return jobs.Job{}, err
				}
			}
		}
	}
	if _, err := b.auth.RequireAdmin(ctx, tx, current); err != nil {
		return jobs.Job{}, err
	}
	event := "cover_batch_enqueued"
	if !created {
		event = "cover_batch_coalesced"
	}
	if err := store.Audit(ctx, tx, actor.User.ID, event, "job", job.ID, map[string]any{"target_count": len(targets)}, now); err != nil {
		return jobs.Job{}, err
	}
	if err := command.Save(ctx, tx, job, now); err != nil {
		return jobs.Job{}, err
	}
	return job, tx.Commit()
}

func (b *AlbumBatches) CoverTargets(ctx context.Context, current auth.Session, id, after string, limit int) ([]CoverBatchTarget, error) {
	if !pageLimit(limit) {
		return nil, ErrCoverBatchSelection
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := b.auth.RequireAdmin(ctx, tx, current); err != nil {
		return nil, err
	}
	var accepted string
	if err := tx.QueryRowContext(ctx, `SELECT job_id FROM album_cover_batches WHERE job_id=?`, id).Scan(&accepted); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT album_id,album_version,library_id,source_id,source_config_version,requested_cover_id,cover_media_id,cover_media_version,cover_revision_id FROM album_cover_batch_targets WHERE job_id=? AND album_id>? ORDER BY album_id LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := []CoverBatchTarget{}
	for rows.Next() {
		var target CoverBatchTarget
		if err := rows.Scan(&target.AlbumID, &target.AlbumVersion, &target.LibraryID, &target.SourceID, &target.SourceConfigVersion, &target.CoverMediaID, &target.ProjectedCoverID, &target.MediaVersion, &target.RevisionID); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func coverBatchTargetTx(ctx context.Context, tx *sql.Tx, job, id string) (CoverBatchTarget, error) {
	var target CoverBatchTarget
	err := tx.QueryRowContext(ctx, `SELECT album_id,album_version,library_id,source_id,source_config_version,requested_cover_id,cover_media_id,cover_media_version,cover_revision_id FROM album_cover_batch_targets WHERE job_id=? AND album_id=?`, job, id).Scan(&target.AlbumID, &target.AlbumVersion, &target.LibraryID, &target.SourceID, &target.SourceConfigVersion, &target.CoverMediaID, &target.ProjectedCoverID, &target.MediaVersion, &target.RevisionID)
	return target, err
}

func coverBatchProjectionMatchesTx(ctx context.Context, tx *sql.Tx, actor string, target CoverBatchTarget) (bool, error) {
	current, err := captureCoverTargetTx(ctx, tx, actor, CoverBatchSelection{AlbumID: target.AlbumID, CoverMediaID: &target.CoverMediaID})
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrAlbumCover) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sameOverride(current.ProjectedCoverID, target.ProjectedCoverID) || !sameOverride(current.RevisionID, target.RevisionID) {
		return false, nil
	}
	return current.MediaVersion == nil && target.MediaVersion == nil || current.MediaVersion != nil && target.MediaVersion != nil && *current.MediaVersion == *target.MediaVersion, nil
}

func applyCoverBatchTargetTx(ctx context.Context, tx *sql.Tx, actor, job, id string, now time.Time) (int64, error) {
	target, err := coverBatchTargetTx(ctx, tx, job, id)
	if err != nil {
		return 0, err
	}
	matches, err := coverBatchProjectionMatchesTx(ctx, tx, actor, target)
	if err != nil {
		return 0, err
	}
	if !matches {
		return 0, ErrAlbumCover
	}
	state, err := scanAlbumMetadata(tx.QueryRowContext(ctx, albumMetadataSelect+` WHERE `+AlbumVisible+` AND a.id=?`, actor, actor, id))
	if err != nil {
		return 0, err
	}
	updated, err := applyAlbumMetadataTx(ctx, tx, actor, state, AlbumMetadataPatch{CoverMediaID: &target.CoverMediaID}, now)
	return updated.Version, err
}
