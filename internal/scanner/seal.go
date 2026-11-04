package scanner

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

// Seal re-walks the completed staged manifest using bounded memory and streamed
// hashes. The second pass catches additions/deletions and equal-stat overwrites,
// not only file mtime differences. ponytail: two streamed passes trade NAS I/O for
// validated correctness; add revision-bound hash reuse only after measurement.
func (s *Inventory) Seal(ctx context.Context, lease jobs.Lease, id string) (Generation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Generation{}, err
	}
	defer tx.Rollback()
	generation, source, root, err := s.require(ctx, tx, lease, id)
	if err != nil {
		return Generation{}, err
	}
	if generation.State != "walking" {
		return Generation{}, ErrGeneration
	}
	if err := tx.Rollback(); err != nil {
		return Generation{}, err
	}
	summary, err := Walk(ctx, root, s.policy, func(file File) error {
		expected, err := readFile(s.db.QueryRowContext(ctx, fileSelect+` WHERE generation_id=? AND relative_path=?`, id, file.RelativePath))
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
		return Generation{}, err
	}
	if summary.Files != generation.FileCount || summary.Bytes != generation.SizeBytes {
		return Generation{}, ErrChanged
	}
	if summary.Files < source.MinimumFiles {
		return Generation{}, ErrMinimumFiles
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return Generation{}, err
	}
	defer tx.Rollback()
	current, _, _, err := s.require(ctx, tx, lease, id)
	if err != nil {
		return Generation{}, err
	}
	if current.State != "walking" || current.Version != generation.Version {
		return Generation{}, ErrGeneration
	}
	now := s.clock()
	current.State, current.Healthy, current.CompletedAt = "ready", true, &now
	if err := s.save(ctx, tx, &current, now); err != nil {
		return Generation{}, err
	}
	if err := store.Audit(ctx, tx, "", "inventory_sealed", "inventory", id, map[string]any{"source_id": current.SourceID, "generation": current.Number, "files": current.FileCount}, now); err != nil {
		return Generation{}, err
	}
	return current, tx.Commit()
}

// Build stages a complete source even for a target scope. Reconciliation may
// later select that scope, but a partial walk cannot satisfy a whole-source
// minimum-file or health guard. No indexed album/media/source-entry row is changed.
func (s *Inventory) Build(ctx context.Context, lease jobs.Lease, sourceID string, configVersion int64, mode, scope string) (Generation, error) {
	ctx, cancel := context.WithTimeout(ctx, s.policy.Timeout)
	defer cancel()
	generation, err := s.Begin(ctx, lease, sourceID, configVersion, mode, scope)
	if err != nil {
		return Generation{}, err
	}
	if generation.State == "ready" {
		return generation, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return generation, err
	}
	_, _, root, err := s.require(ctx, tx, lease, generation.ID)
	tx.Rollback()
	if err != nil {
		return generation, err
	}
	batch := make([]File, 0, s.policy.BatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		next, err := s.Record(ctx, lease, generation.ID, batch)
		if err != nil {
			return err
		}
		generation = next
		batch = batch[:0]
		return nil
	}
	_, err = Walk(ctx, root, s.policy, func(file File) error {
		batch = append(batch, file)
		if len(batch) == s.policy.BatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return generation, err
	}
	if err := flush(); err != nil {
		return generation, err
	}
	sealed, err := s.Seal(ctx, lease, generation.ID)
	if err != nil {
		return generation, err
	}
	return sealed, nil
}

var inventoryFailureCodes = map[string]bool{
	"SOURCE_UNAVAILABLE": true, "SOURCE_CHANGED": true, "IO_ERROR": true,
	"MINIMUM_FILES": true, "SCAN_LIMIT": true, "UNSAFE_ENTRY": true, "SYMLINK_CYCLE": true,
}

// Abort records safe per-path diagnostics and one failed verification work unit.
// It cannot resurrect a cancelled, expired, superseded, or configuration-stale
// attempt; the next authorized generation reclaims that abandoned staging state.
func (s *Inventory) Abort(ctx context.Context, lease jobs.Lease, id, code, relative string) error {
	if !inventoryFailureCodes[code] || (relative != "." && !validRelative(relative)) {
		return ErrGeneration
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	generation, _, _, err := s.require(ctx, tx, lease, id)
	if err != nil {
		return err
	}
	job, err := s.queue.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return err
	}
	if job.Total == math.MaxInt64 || job.Done == math.MaxInt64 || job.Failed == math.MaxInt64 {
		return ErrLimit
	}
	now := s.clock()
	generation.State, generation.Healthy, generation.ErrorCode, generation.CompletedAt = "failed", false, &code, &now
	if _, err := tx.ExecContext(ctx, `INSERT INTO inventory_issues(id,generation_id,relative_path,error_code,created_at) VALUES(?,?,?,?,?)`, store.NewID(), id, relative, code, now.UnixMilli()); err != nil {
		return err
	}
	if err := s.save(ctx, tx, &generation, now); err != nil {
		return err
	}
	counts := job.Counts
	counts.Total++
	counts.Done++
	counts.Failed++
	if _, err := s.queue.ProgressTx(ctx, tx, lease, counts, false); err != nil {
		return err
	}
	if err := store.Audit(ctx, tx, "", "inventory_failed", "inventory", id, map[string]any{"source_id": generation.SourceID, "error_code": code}, now); err != nil {
		return err
	}
	return tx.Commit()
}
