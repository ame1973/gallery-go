package scanner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"time"
	"unicode/utf8"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

// Handler executes only versioned, durably selected scan intents. Queue execution
// supplies independent heartbeats/cancellation; every data publication still uses
// the inventory's own current source/config/generation/lease transaction fences.
func (s *Scans) Handler(inventory *Inventory, policy media.InspectPolicy) (jobs.Handler, error) {
	if inventory == nil || inventory.db != s.db || inventory.queue != s.queue || inventory.sources == nil {
		return nil, ErrPolicy
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return func(ctx context.Context, lease jobs.Lease) error {
		mode, count, err := s.scanJob(ctx, lease)
		if err != nil {
			return err
		}
		var processed int64
		after := ""
		failed := false
		retryCode := ""
		for {
			targets, err := s.scanBatch(ctx, lease, after)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				break
			}
			for _, target := range targets {
				processed++
				if err := ctx.Err(); err != nil {
					return err
				}
				sourceCtx, cancel := context.WithTimeout(ctx, inventory.policy.Timeout)
				generation, err := s.executeSource(sourceCtx, lease, inventory, policy, target, mode, processed == count && !failed)
				cancel()
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if errors.Is(err, jobs.ErrCancelled) || errors.Is(err, jobs.ErrLeaseLost) {
						return err
					}
					code, jobCode, retryable := scanFailure(err)
					if err := s.recordSourceFailure(ctx, lease, inventory, target, generation, code, jobCode, err); err != nil {
						return err
					}
					failed = true
					if retryable && retryCode == "" {
						retryCode = jobCode
					}
				}
			}
			if processed == count {
				break
			}
			after = targets[len(targets)-1].sourceID
		}
		if processed != count {
			return &jobs.Failure{Code: "UNSUPPORTED_JOB"}
		}
		if !failed {
			return nil
		} // The final successful source committed terminal state.
		if retryCode != "" {
			return &jobs.Failure{Code: retryCode, Retryable: true}
		}
		return s.completeScan(ctx, lease)
	}, nil
}

func (s *Scans) scanJob(ctx context.Context, lease jobs.Lease) (string, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback()
	job, err := s.queue.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return "", 0, err
	}
	invalid := func() (string, int64, error) { return "", 0, &jobs.Failure{Code: "UNSUPPORTED_JOB"} }
	if job.Kind != "scan" || !utf8.Valid(job.Payload) {
		return invalid()
	}
	var payload scanPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return invalid()
	}
	// Private v1 bytes are emitted canonically by Enqueue. Reject duplicate,
	// unknown, null or noncanonical fields instead of guessing a legacy intent.
	canonical, _ := json.Marshal(payload)
	if !bytes.Equal(canonical, job.Payload) || payload.Version != 1 || (payload.Mode != "full" && payload.Mode != "incremental") {
		return invalid()
	}
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM scan_targets WHERE job_id=?`, job.ID).Scan(&count); err != nil {
		return "", 0, err
	}
	if count < 1 {
		return invalid()
	}
	return payload.Mode, count, nil
}

func (s *Scans) scanBatch(ctx context.Context, lease jobs.Lease, after string) ([]scanTarget, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := s.queue.RequireLeaseTx(ctx, tx, lease); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.source_id,t.source_config_version,t.scope_path,t.album_id,EXISTS(SELECT 1 FROM job_resources r WHERE r.job_id=t.job_id AND r.resource_key='source:'||t.source_id) FROM scan_targets t WHERE t.job_id=? AND t.source_id>? ORDER BY t.source_id LIMIT 100`, lease.Job.ID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make([]scanTarget, 0, 100)
	for rows.Next() {
		var target scanTarget
		var bound bool
		if err := rows.Scan(&target.sourceID, &target.config, &target.scope, &target.album, &bound); err != nil {
			return nil, err
		}
		if !bound || target.sourceID == "" || len(target.sourceID) > 64 || !utf8.ValidString(target.sourceID) || target.config < 1 || (target.scope != "." && !validRelative(target.scope)) || (target.album.Valid && (target.album.String == "" || len(target.album.String) > 64 || !utf8.ValidString(target.album.String))) {
			return nil, &jobs.Failure{Code: "UNSUPPORTED_JOB"}
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (s *Scans) executeSource(ctx context.Context, lease jobs.Lease, inventory *Inventory, policy media.InspectPolicy, target scanTarget, mode string, final bool) (Generation, error) {
	if target.album.Valid {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return Generation{}, err
		}
		if _, err := s.queue.RequireLeaseTx(ctx, tx, lease); err != nil {
			tx.Rollback()
			return Generation{}, err
		}
		var source, path sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT source_id,relative_path FROM albums WHERE id=?`, target.album).Scan(&source, &path)
		tx.Rollback()
		if err != nil {
			return Generation{}, err
		}
		if !source.Valid || !path.Valid || source.String != target.sourceID || path.String != target.scope {
			return Generation{}, library.ErrSourceChanged
		}
	}
	var generation Generation
	var err error
	for {
		generation, err = inventory.Build(ctx, lease, target.sourceID, target.config, mode, target.scope)
		if !errors.Is(err, ErrActiveGeneration) {
			break
		}
		// A distinct intent may wait for the source's one active generation. It
		// retains a real lease, not fabricated progress or exhausted retry attempts.
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return generation, ctx.Err()
		case <-timer.C:
		}
	}
	if err != nil {
		return generation, err
	}
	staged, err := inventory.ProbeMetadata(ctx, lease, generation.ID, policy)
	if err != nil {
		return generation, err
	}
	generation = staged
	var published Generation
	if final {
		published, err = inventory.Publish(ctx, lease, generation.ID)
	} else {
		published, err = inventory.PublishPart(ctx, lease, generation.ID)
	}
	if err != nil {
		return generation, err
	}
	return published, nil
}

func scanFailure(err error) (string, string, bool) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "IO_ERROR", "JOB_TIMEOUT", true
	case errors.Is(err, ErrMinimumFiles):
		return "MINIMUM_FILES", "PROCESSING_FAILED", false
	case errors.Is(err, ErrLimit):
		return "SCAN_LIMIT", "PROCESSING_FAILED", false
	case errors.Is(err, ErrCycle):
		return "SYMLINK_CYCLE", "PROCESSING_FAILED", false
	case errors.Is(err, ErrUnsafeEntry):
		return "UNSAFE_ENTRY", "INVALID_MEDIA", false
	case errors.Is(err, ErrChanged), errors.Is(err, media.ErrContentChanged), errors.Is(err, library.ErrSourceChanged), errors.Is(err, ErrGeneration):
		return "SOURCE_CHANGED", "SOURCE_CHANGED", true
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission), errors.Is(err, media.ErrSourceUnavailable):
		return "SOURCE_UNAVAILABLE", "SOURCE_UNAVAILABLE", true
	default:
		return "IO_ERROR", "IO_ERROR", true
	}
}

func (s *Scans) recordSourceFailure(ctx context.Context, lease jobs.Lease, inventory *Inventory, target scanTarget, generation Generation, code, jobCode string, cause error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := s.queue.RequireLeaseTx(ctx, tx, lease); err != nil {
		return err
	}
	path := "."
	var failure *PathError
	if errors.As(cause, &failure) && (failure.Path == "." || validRelative(failure.Path)) {
		path = failure.Path
	}
	if generation.ID != "" {
		if err := inventory.AbortTx(ctx, tx, lease, generation.ID, code, path); err != nil {
			return err
		}
	} else {
		inserted, err := tx.ExecContext(ctx, `INSERT INTO job_issues(id,job_id,attempt,source_id,source_config_version,generation_id,relative_path,error_code,created_at) VALUES(?,?,?,?,?,'',?,?,?) ON CONFLICT(job_id,attempt,source_id,generation_id,relative_path,error_code) DO NOTHING`, store.NewID(), lease.Job.ID, lease.Job.Attempts, target.sourceID, target.config, path, code, s.clock().UnixMilli())
		if err != nil {
			return err
		}
		count, err := inserted.RowsAffected()
		if err != nil {
			return err
		}
		if count != 0 {
			if err := inventory.workProgress(ctx, tx, lease, 0, 1, 0); err != nil {
				return err
			}
		}
	}
	if err := inventory.sources.RecordScanFailureTx(ctx, tx, target.sourceID, target.config, code); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := store.Audit(ctx, tx, "", "scan_target_failed", "job", lease.Job.ID, map[string]any{"source_id": target.sourceID, "error_code": jobCode, "generation_id": generation.ID}, s.clock()); err != nil {
		return err
	}
	if _, err := s.queue.RequireLeaseTx(ctx, tx, lease); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Scans) completeScan(ctx context.Context, lease jobs.Lease) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := s.queue.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return err
	}
	if _, err := s.queue.ProgressTx(ctx, tx, lease, job.Counts, true); err != nil {
		return err
	}
	if _, err := s.queue.CompleteTx(ctx, tx, lease); err != nil {
		return err
	}
	return tx.Commit()
}
