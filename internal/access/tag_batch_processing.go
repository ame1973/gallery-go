package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

// Process shares lease/budget/result mechanics for exactly two metadata operations.
// Concrete target schemas and setters remain independent; no request selects SQL.
func (b *AlbumBatches) Process(ctx context.Context, lease jobs.Lease) error {
	ids, err := b.beginTagBatch(ctx, lease)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := b.processTagBatchTarget(ctx, lease, id); err != nil {
			return err
		}
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := b.queue.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return err
	}
	tables, ok := tablesForAlbumBatch(job.Kind)
	if !ok {
		return jobs.ErrInvalidJob
	}
	var expected, recorded int
	var actor string
	if err := tx.QueryRowContext(ctx, `SELECT r.target_count,(SELECT COUNT(*) FROM `+tables.results+` WHERE job_id=r.job_id AND attempt=r.attempt),b.actor_user_id FROM `+tables.runs+` r JOIN `+tables.intent+` b ON b.job_id=r.job_id WHERE r.job_id=? AND r.attempt=?`, job.ID, job.Attempts).Scan(&expected, &recorded, &actor); err != nil {
		return err
	}
	if expected != recorded {
		return jobs.ErrProgress
	}
	if err := store.Audit(ctx, tx, actor, tables.audit+"_finished", "job", job.ID, map[string]any{"attempt": job.Attempts, "target_count": expected}, b.now().UTC()); err != nil {
		return err
	}
	if _, err := b.queue.CompleteTx(ctx, tx, lease); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *AlbumBatches) beginTagBatch(ctx context.Context, lease jobs.Lease) ([]string, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, err := b.queue.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return nil, err
	}
	tables, ok := tablesForAlbumBatch(job.Kind)
	if !ok || string(job.Payload) != `{"version":1}` {
		return nil, &jobs.Failure{Code: "UNSUPPORTED_JOB"}
	}
	var accepted string
	if err := tx.QueryRowContext(ctx, `SELECT job_id FROM `+tables.intent+` WHERE job_id=?`, job.ID).Scan(&accepted); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT album_id FROM `+tables.targets+` WHERE job_id=? ORDER BY album_id LIMIT 101`, job.ID)
	if err != nil {
		return nil, err
	}
	ids, err := scanGroupIDs(rows)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, ErrTagBatchSelection
	}
	if err := b.queue.HeartbeatTx(ctx, tx, lease); err != nil {
		return nil, err
	}
	created, err := tx.ExecContext(ctx, `INSERT INTO `+tables.runs+`(job_id,attempt,target_count,created_at) VALUES(?,?,?,?) ON CONFLICT(job_id,attempt) DO NOTHING`, job.ID, job.Attempts, len(ids), b.now().UTC().UnixMilli())
	if err != nil {
		return nil, err
	}
	count, err := created.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count == 1 {
		if job.Total > math.MaxInt64-int64(len(ids)) {
			return nil, jobs.ErrProgress
		}
		counts := job.Counts
		counts.Total += int64(len(ids))
		if _, err := b.queue.ProgressTx(ctx, tx, lease, counts, true); err != nil {
			return nil, err
		}
	} else {
		var reserved int
		if err := tx.QueryRowContext(ctx, `SELECT target_count FROM `+tables.runs+` WHERE job_id=? AND attempt=?`, job.ID, job.Attempts).Scan(&reserved); err != nil {
			return nil, err
		}
		if !job.TotalKnown || reserved != len(ids) {
			return nil, jobs.ErrProgress
		}
	}
	if _, err := b.queue.RequireLeaseTx(ctx, tx, lease); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}

func tagBatchTargetMatchesTx(ctx context.Context, tx *sql.Tx, actor string, target TagBatchTarget, version int64) (bool, error) {
	var matches bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM albums a JOIN libraries l ON l.id=a.library_id LEFT JOIN sources s ON s.id=a.source_id WHERE `+AlbumVisible+` AND a.id=? AND a.version=? AND a.library_id=? AND a.source_id IS ? AND s.config_version IS ?)`, actor, target.AlbumID, version, target.LibraryID, target.SourceID, target.SourceConfigVersion).Scan(&matches)
	return matches, err
}

func applyTagBatchTargetTx(ctx context.Context, tx *sql.Tx, actor, job, id string, now time.Time) (int64, error) {
	var data string
	if err := tx.QueryRowContext(ctx, `SELECT tag_ids_json FROM album_tag_batches WHERE job_id=?`, job).Scan(&data); err != nil {
		return 0, err
	}
	var ids []string
	if err := json.Unmarshal([]byte(data), &ids); err != nil {
		return 0, err
	}
	ids, data, err := normalizedGroups(ids)
	if err != nil {
		return 0, ErrTagSet
	}
	current, err := albumTagSetInTx(ctx, tx, actor, id)
	if err != nil {
		return 0, err
	}
	changed, err := replaceAlbumTagsTx(ctx, tx, actor, current, ids, data, now)
	return changed.Version, err
}

func (b *AlbumBatches) processTagBatchTarget(ctx context.Context, lease jobs.Lease, id string) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := b.queue.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return err
	}
	tables, ok := tablesForAlbumBatch(job.Kind)
	if !ok {
		return jobs.ErrInvalidJob
	}
	var reserved int
	if err := tx.QueryRowContext(ctx, `SELECT target_count FROM `+tables.runs+` WHERE job_id=? AND attempt=?`, job.ID, job.Attempts).Scan(&reserved); err != nil {
		return err
	}
	if !job.TotalKnown {
		return jobs.ErrProgress
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+tables.results+` WHERE job_id=? AND attempt=? AND album_id=?)`, job.ID, job.Attempts, id).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if job.Done >= job.Total {
		return jobs.ErrProgress
	}
	var actor string
	var target TagBatchTarget
	if err := tx.QueryRowContext(ctx, `SELECT b.actor_user_id,t.album_id,t.album_version,t.library_id,t.source_id,t.source_config_version FROM `+tables.intent+` b JOIN `+tables.targets+` t ON t.job_id=b.job_id WHERE b.job_id=? AND t.album_id=?`, job.ID, id).Scan(&actor, &target.AlbumID, &target.AlbumVersion, &target.LibraryID, &target.SourceID, &target.SourceConfigVersion); err != nil {
		return err
	}
	if err := b.queue.HeartbeatTx(ctx, tx, lease); err != nil {
		return err
	}
	now := b.now().UTC().Truncate(time.Millisecond)
	result := AlbumBatchResult{ID: store.NewID(), AlbumID: id, Attempt: job.Attempts, CreatedAt: now}
	var previousVersion int64
	err = tx.QueryRowContext(ctx, `SELECT album_version FROM `+tables.results+` WHERE job_id=? AND album_id=? AND outcome IN ('applied','unchanged')`, job.ID, id).Scan(&previousVersion)
	checkActor, checkTarget := false, false
	switch {
	case err == nil:
		// Prior resolution is historical fact, not permission to overwrite a later
		// edit or reapply against a rewritten/moved/deleted media object.
		result.Outcome, result.AlbumVersion = "already_applied", &previousVersion
	case !errors.Is(err, sql.ErrNoRows):
		return err
	default:
		_, err := b.auth.RequireActiveAdminTx(ctx, tx, actor)
		if errors.Is(err, auth.ErrForbidden) {
			result.fail("ACTOR_NOT_AUTHORIZED")
		} else if err != nil {
			return err
		} else {
			checkActor = true
			matches, err := tagBatchTargetMatchesTx(ctx, tx, actor, target, target.AlbumVersion)
			if err != nil {
				return err
			}
			if !matches {
				result.fail("TARGET_CHANGED")
			} else {
				var version int64
				if job.Kind == "batch_tags" {
					version, err = applyTagBatchTargetTx(ctx, tx, actor, job.ID, id, now)
				} else {
					version, err = applyCoverBatchTargetTx(ctx, tx, actor, job.ID, id, now)
				}
				switch {
				case errors.Is(err, ErrUnknownTag):
					result.fail("TAG_NOT_FOUND")
				case errors.Is(err, ErrAlbumCover):
					result.fail("COVER_CHANGED")
				case errors.Is(err, auth.ErrConflict):
					result.fail("TARGET_CHANGED")
				case err != nil:
					return err
				default:
					checkTarget = true
					result.Outcome = "unchanged"
					if version != target.AlbumVersion {
						result.Outcome = "applied"
					}
					result.AlbumVersion = &version
				}
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+tables.results+`(id,job_id,attempt,album_id,outcome,error_code,album_version,created_at) VALUES(?,?,?,?,?,?,?,?)`, result.ID, job.ID, job.Attempts, id, result.Outcome, result.ErrorCode, result.AlbumVersion, now.UnixMilli()); err != nil {
		return err
	}
	counts := job.Counts
	counts.Done++
	switch result.Outcome {
	case "applied":
		counts.Succeeded++
	case "failed":
		counts.Failed++
	default:
		counts.Skipped++
	}
	if _, err := b.queue.ProgressTx(ctx, tx, lease, counts, true); err != nil {
		return err
	}
	if err := store.Audit(ctx, tx, actor, tables.audit+"_target_processed", "job", job.ID, map[string]any{"album_id": id, "attempt": job.Attempts, "outcome": result.Outcome, "error_code": result.ErrorCode, "album_version": result.AlbumVersion}, now); err != nil {
		return err
	}
	if checkActor {
		if _, err := b.auth.RequireActiveAdminTx(ctx, tx, actor); err != nil {
			return err
		}
	}
	if checkTarget {
		matches, err := tagBatchTargetMatchesTx(ctx, tx, actor, target, *result.AlbumVersion)
		if err != nil {
			return err
		}
		if !matches {
			return auth.ErrConflict
		}
		if job.Kind == "batch_covers" {
			frozen, err := coverBatchTargetTx(ctx, tx, job.ID, id)
			if err != nil {
				return err
			}
			matches, err := coverBatchProjectionMatchesTx(ctx, tx, actor, frozen)
			if err != nil {
				return err
			}
			if !matches {
				return ErrAlbumCover
			}
		}
	}
	if _, err := b.queue.RequireLeaseTx(ctx, tx, lease); err != nil {
		return err
	}
	return tx.Commit()
}

var albumBatchMessages = map[string]string{
	"ACTOR_NOT_AUTHORIZED": "The original administrator account is no longer active and authorized.",
	"TARGET_CHANGED":       "The target or its source changed or is no longer available; create a new intent after reviewing it.",
	"TAG_NOT_FOUND":        "One or more accepted tag IDs no longer exist.",
	"COVER_CHANGED":        "The accepted cover projection or media version changed or is no longer eligible.",
	"PROCESSING_FAILED":    "The target did not complete successfully.",
}

// AlbumBatchResult is a committed attempt outcome, never a current content grant.
// AlbumVersion is a resolved historical version, including already-applied skips.
type AlbumBatchResult struct {
	ID           string    `json:"id"`
	AlbumID      string    `json:"album_id"`
	Attempt      int       `json:"attempt"`
	Outcome      string    `json:"outcome"`
	AlbumVersion *int64    `json:"album_version"`
	ErrorCode    *string   `json:"error_code"`
	Message      *string   `json:"message"`
	CreatedAt    time.Time `json:"created_at"`
}

func (r *AlbumBatchResult) fail(code string) {
	message, ok := albumBatchMessages[code]
	if !ok {
		code = "PROCESSING_FAILED"
		message = albumBatchMessages[code]
	}
	r.Outcome, r.ErrorCode, r.Message, r.AlbumVersion = "failed", &code, &message, nil
}

func (b *AlbumBatches) Results(ctx context.Context, current auth.Session, id, after string, limit int) ([]AlbumBatchResult, error) {
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
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM jobs WHERE id=?`, id).Scan(&kind); err != nil {
		return nil, err
	}
	tables, ok := tablesForAlbumBatch(kind)
	if !ok {
		return nil, sql.ErrNoRows
	}
	var found string
	if err := tx.QueryRowContext(ctx, `SELECT job_id FROM `+tables.intent+` WHERE job_id=?`, id).Scan(&found); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,album_id,attempt,outcome,album_version,error_code,created_at FROM `+tables.results+` WHERE job_id=? AND id>? ORDER BY id LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := []AlbumBatchResult{}
	for rows.Next() {
		var result AlbumBatchResult
		var created int64
		if err := rows.Scan(&result.ID, &result.AlbumID, &result.Attempt, &result.Outcome, &result.AlbumVersion, &result.ErrorCode, &created); err != nil {
			return nil, err
		}
		result.CreatedAt = time.UnixMilli(created).UTC()
		if result.Outcome == "failed" {
			code := "PROCESSING_FAILED"
			if result.ErrorCode != nil {
				code = *result.ErrorCode
			}
			result.fail(code)
		}
		results = append(results, result)
	}
	return results, rows.Err()
}
