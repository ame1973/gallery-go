package scanner

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"gallerygo/internal/library"
	"gallerygo/internal/store"
)

type SchedulePolicy struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
}

func DefaultSchedulePolicy() SchedulePolicy {
	return SchedulePolicy{Enabled: true, Interval: time.Second}
}
func (p SchedulePolicy) Validate() error {
	if p.Interval < time.Second || p.Interval > time.Minute {
		return ErrPolicy
	}
	return nil
}

// ScheduleDue freezes at most 100 due, active, process-configured source intents in
// one writer. Private dispatch deadlines, job targets/dependencies, outbox and system
// audit commit together. No filesystem probe or administrator credential is used.
// The returned cursor is a live keyset boundary, not a fixed inventory snapshot.
func (s *Scans) ScheduleDue(ctx context.Context, sources *library.Sources, after string) (string, int, error) {
	if sources == nil {
		return "", 0, ErrPolicy
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback()
	now := s.clock()
	type dueSource struct {
		id               string
		config, nextFull int64
		lastFull         sql.NullInt64
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,config_version,next_full_scan_at,last_full_scan_at FROM sources WHERE is_active=1 AND (next_scan_at<=? OR next_full_scan_at<=?) AND id>? ORDER BY id LIMIT 100`, now.UnixMilli(), now.UnixMilli(), after)
	if err != nil {
		return "", 0, err
	}
	batch := make([]dueSource, 0, 100)
	for rows.Next() {
		var source dueSource
		if err := rows.Scan(&source.id, &source.config, &source.nextFull, &source.lastFull); err != nil {
			rows.Close()
			return "", 0, err
		}
		batch = append(batch, source)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", 0, err
	}
	if len(batch) == 0 {
		return "", 0, nil
	}
	scheduled := 0
	for _, due := range batch {
		source, _, err := sources.RequireConfigTx(ctx, tx, due.id, due.config)
		if errors.Is(err, library.ErrSourceChanged) {
			continue
		}
		if err != nil {
			return "", 0, err
		}
		mode := "incremental"
		if !due.lastFull.Valid || due.nextFull <= now.UnixMilli() {
			mode = "full"
		}
		job, created, _, err := s.enqueueTx(ctx, tx, ScanRequest{SourceID: &due.id, Mode: mode}, "")
		if err != nil {
			return "", 0, err
		}
		// Keep independent full/poll wakeups: an overdue full scan cannot wait for
		// a later poll. A failed/coalesced full retries at the poll cadence, never
		// spins each scheduler tick. LastFullScanAt remains healthy-publication-only.
		next := now.Add(time.Duration(source.PollIntervalSeconds) * time.Second).UnixMilli()
		if _, err := tx.ExecContext(ctx, `UPDATE sources SET next_scan_at=?,next_full_scan_at=CASE WHEN ? THEN ? ELSE next_full_scan_at END WHERE id=? AND config_version=? AND is_active=1`, next, mode == "full", next, source.ID, source.ConfigVersion); err != nil {
			return "", 0, err
		}
		event := "scan_scheduled"
		if !created {
			event = "scan_schedule_coalesced"
		}
		if err := store.Audit(ctx, tx, "", event, "job", job.ID, map[string]any{"source_id": source.ID, "config_version": source.ConfigVersion, "mode": mode}, now); err != nil {
			return "", 0, err
		}
		scheduled++
	}
	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	return batch[len(batch)-1].id, scheduled, nil
}

// RunSchedule is safe to run in several worker processes: SQLite serializes due
// selection and deadline advancement, and identical manual/scheduled intents use
// the same immutable coalescing key. Genuine DB faults stop the owning runtime.
func (s *Scans) RunSchedule(ctx context.Context, sources *library.Sources, policy SchedulePolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if sources == nil {
		return ErrPolicy
	}
	if !policy.Enabled {
		return nil
	}
	ticker := time.NewTicker(policy.Interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		after := ""
		for ctx.Err() == nil {
			batchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			next, _, err := s.ScheduleDue(batchCtx, sources, after)
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if store.IsBusy(err) || errors.Is(err, context.DeadlineExceeded) {
				break
			}
			if err != nil {
				return err
			}
			if next == "" {
				break
			}
			after = next
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
	return nil
}
