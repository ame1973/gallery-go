package library

import (
	"context"
	"database/sql"
	"time"

	"gallerygo/internal/store"
)

// PublishInventoryTx is paired with the scanner's live job/generation fence in
// the same transaction. It publishes health only after complete indexed work;
// reachability checks alone never advance this generation or scheduling evidence.
func (s *Sources) PublishInventoryTx(ctx context.Context, tx *sql.Tx, id string, configVersion, generation int64, full bool) (Source, error) {
	source, _, err := s.RequireConfigTx(ctx, tx, id, configVersion)
	if err != nil {
		return Source{}, err
	}
	if generation <= source.ScanGeneration {
		return Source{}, ErrSourceChanged
	}
	now := s.clock()
	source.ScanGeneration, source.State, source.LastErrorCode = generation, "healthy", nil
	source.Version++
	source.UpdatedAt = maxTime(source.UpdatedAt, now)
	checked, healthy := now, now
	if source.LastCheckedAt != nil {
		checked = maxTime(*source.LastCheckedAt, now)
	}
	if source.LastHealthyAt != nil {
		healthy = maxTime(*source.LastHealthyAt, now)
	}
	source.LastCheckedAt, source.LastHealthyAt = &checked, &healthy
	if full {
		last := now
		if source.LastFullScanAt != nil {
			last = maxTime(*source.LastFullScanAt, now)
		}
		source.LastFullScanAt = &last
	}
	var lastFull any
	if source.LastFullScanAt != nil {
		lastFull = source.LastFullScanAt.UnixMilli()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sources SET scan_generation=?,state='healthy',last_error_code=NULL,version=?,updated_at=?,last_checked_at=?,last_healthy_at=?,last_full_scan_at=?,next_scan_at=?,next_full_scan_at=CASE WHEN ? THEN ? ELSE next_full_scan_at END WHERE id=?`, generation, source.Version, source.UpdatedAt.UnixMilli(), checked.UnixMilli(), healthy.UnixMilli(), lastFull, now.Add(time.Duration(source.PollIntervalSeconds)*time.Second).UnixMilli(), full, now.Add(time.Duration(source.FullScanIntervalSeconds)*time.Second).UnixMilli(), id); err != nil {
		return Source{}, err
	}
	if err := store.Change(ctx, tx, "system", "source", id, "upsert", source.Version, source, now); err != nil {
		return Source{}, err
	}
	return source, nil
}
