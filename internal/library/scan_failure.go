package library

import (
	"context"
	"database/sql"

	"gallerygo/internal/store"
)

var scanFailureCodes = map[string]bool{
	"SOURCE_UNAVAILABLE": true, "SOURCE_CHANGED": true, "SOURCE_NOT_CONFIGURED": true,
	"IO_ERROR": true, "MINIMUM_FILES": true, "SCAN_LIMIT": true, "UNSAFE_ENTRY": true, "SYMLINK_CYCLE": true,
}

// RecordScanFailureTx is paired with the caller's live lease fence. Failures may
// update reachability/diagnostics, never the published generation or indexed rows.
// Even a process with an obsolete registry cannot bless its mismatched root pin.
func (s *Sources) RecordScanFailureTx(ctx context.Context, tx *sql.Tx, id string, configVersion int64, code string) error {
	if !scanFailureCodes[code] {
		return ErrSourceSettings
	}
	source, err := s.read(tx.QueryRowContext(ctx, sourceSelect+` WHERE id=?`, id))
	if err != nil {
		return err
	}
	if !source.Active || source.ConfigVersion != configVersion {
		return ErrSourceChanged
	}
	if !source.Configured {
		code = "SOURCE_NOT_CONFIGURED"
	}
	state := "error"
	if code == "SOURCE_UNAVAILABLE" || code == "SOURCE_NOT_CONFIGURED" {
		state = "unavailable"
	}
	now := s.clock()
	checked := now
	if source.LastCheckedAt != nil {
		checked = maxTime(*source.LastCheckedAt, now)
	}
	source.State, source.LastErrorCode, source.LastCheckedAt = state, &code, &checked
	source.Version++
	source.UpdatedAt = maxTime(source.UpdatedAt, now)
	if _, err := tx.ExecContext(ctx, `UPDATE sources SET state=?,last_error_code=?,last_checked_at=?,version=?,updated_at=? WHERE id=?`, state, code, checked.UnixMilli(), source.Version, source.UpdatedAt.UnixMilli(), id); err != nil {
		return err
	}
	if err := store.Change(ctx, tx, "system", "source", id, "upsert", source.Version, source, now); err != nil {
		return err
	}
	return store.Audit(ctx, tx, "", "source_scan_failed", "source", id, map[string]any{"error_code": code, "config_version": configVersion}, now)
}
