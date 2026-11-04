package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

// ProbeRoot checks root/marker reachability only, not a complete inventory or a
// minimum-file guard. It never creates a root, marker, source file, or directory.
// Kernel NAS stalls are not made cancellable by a context; deployment must bound
// those separately. Scanners still require a completed healthy generation.
func ProbeRoot(ctx context.Context, root media.SourceRoot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validRoot(root) {
		return ErrUnknownRoot
	}
	directory, err := os.OpenRoot(root.Path)
	if err != nil {
		return err
	}
	defer directory.Close()
	marker, _, err := media.OpenReadOnlyFile(directory, root.Marker)
	if err != nil {
		return err
	}
	_, err = marker.Read(make([]byte, 1))
	marker.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	folder, err := directory.Open(".")
	if err != nil {
		return err
	}
	_, err = folder.ReadDir(1)
	folder.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return ctx.Err()
}

func (s *Sources) Check(ctx context.Context, current auth.Session, id string, version int64, key string) (Source, error) {
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return Source{}, err
	}
	defer tx.Rollback()
	now := s.clock()
	metadata, _ := json.Marshal(struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}{id, version})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "source.check", Metadata: string(metadata)}
	var source Source
	if found, err := command.Replay(ctx, tx, now, &source); found || err != nil {
		return source, err
	}
	source, err = s.read(tx.QueryRowContext(ctx, sourceSelect+` WHERE id=?`, id))
	if err != nil {
		return Source{}, err
	}
	if source.Version != version {
		return Source{}, auth.ErrConflict
	}
	if err := tx.Rollback(); err != nil {
		return Source{}, err
	}
	state := "healthy"
	var code *string
	if !source.Configured {
		value := "SOURCE_NOT_CONFIGURED"
		code = &value
		state = "unavailable"
	} else {
		root, err := s.root(source.RootName)
		if err != nil {
			return Source{}, err
		}
		if err := ProbeRoot(ctx, root); err != nil {
			if ctx.Err() != nil {
				return Source{}, ctx.Err()
			}
			slog.Warn("source reachability check failed", "source_id", id, "error", err)
			value := "SOURCE_UNAVAILABLE"
			code = &value
			state = "unavailable"
		}
	}
	// Filesystem I/O has ended. Reauthorize and recheck the version before writing
	// reachability, so a revoked administrator or changed root cannot publish it.
	tx, actor, err = s.adminTx(ctx, current)
	if err != nil {
		return Source{}, err
	}
	defer tx.Rollback()
	now = s.clock()
	if found, err := command.Replay(ctx, tx, now, &source); found || err != nil {
		return source, err
	}
	source, err = s.read(tx.QueryRowContext(ctx, sourceSelect+` WHERE id=?`, id))
	if err != nil {
		return Source{}, err
	}
	if source.Version != version {
		return Source{}, auth.ErrConflict
	}
	source.Version++
	source.UpdatedAt = maxTime(source.UpdatedAt, now)
	checked := now
	if source.LastCheckedAt != nil {
		checked = maxTime(*source.LastCheckedAt, now)
	}
	source.LastCheckedAt, source.State, source.LastErrorCode = &checked, state, code
	if state == "healthy" {
		healthy := now
		if source.LastHealthyAt != nil {
			healthy = maxTime(*source.LastHealthyAt, now)
		}
		source.LastHealthyAt = &healthy
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sources SET state=?,last_error_code=?,last_checked_at=?,last_healthy_at=?,version=?,updated_at=? WHERE id=?`, source.State, source.LastErrorCode, checked.UnixMilli(), sourceTimestamp(source.LastHealthyAt), source.Version, source.UpdatedAt.UnixMilli(), id); err != nil {
		return Source{}, err
	}
	if err := store.Change(ctx, tx, "system", "source", id, "upsert", source.Version, source, now); err != nil {
		return Source{}, err
	}
	if err := store.Audit(ctx, tx, actor.ID, "source_checked", "source", id, map[string]any{"state": state, "error_code": code, "config_version": source.ConfigVersion}, now); err != nil {
		return Source{}, err
	}
	if err := command.Save(ctx, tx, source, now); err != nil {
		return Source{}, err
	}
	if err := tx.Commit(); err != nil {
		return Source{}, fmt.Errorf("commit source check: %w", err)
	}
	return source, nil
}

func sourceTimestamp(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UnixMilli()
}
