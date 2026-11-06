//go:build linux && (amd64 || arm64)

package media

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"time"

	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func artifactMaintenanceEvents(kind string) (scope, finished string, err error) {
	switch kind {
	case "reconcile_temps":
		return "artifact_temp_scope_checked", "artifact_temps_reconciled", nil
	case "reconcile_artifact":
		return "artifact_orphan_scope_checked", "artifact_orphans_reconciled", nil
	default:
		return "", "", errArtifactTemps
	}
}

// Only the two compiled maintenance families share DB-only queue mechanics.
// Callers construct exact typed work identities; no public request selects SQL,
// audit names, callbacks or filesystem behavior.
func (s *Service) requireArtifactMaintenanceTx(ctx context.Context, tx *sql.Tx, q *jobs.Queue, lease jobs.Lease, input jobs.Input, resources ...string) (jobs.Job, error) {
	job, err := q.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return jobs.Job{}, err
	}
	if err := s.requireArtifactMaintenanceStateTx(ctx, tx, job, input, resources...); err != nil {
		return jobs.Job{}, err
	}
	return job, nil
}

// Also fence terminal queue writes without reviving or reusing a completed lease.
func (s *Service) requireArtifactMaintenanceStateTx(ctx context.Context, tx *sql.Tx, job jobs.Job, input jobs.Input, resources ...string) error {
	if (input.Kind == "reconcile_temps" && len(resources) != 0) || (input.Kind == "reconcile_artifact" && len(resources) != 2) {
		return errArtifactTemps
	}
	if _, _, err := artifactMaintenanceEvents(input.Kind); err != nil {
		return err
	}
	if input.Kind == "reconcile_artifact" && (!strings.HasPrefix(resources[0], "media:") || len(resources[0]) <= len("media:") || !strings.HasPrefix(resources[1], "revision:") || len(resources[1]) <= len("revision:")) {
		return errArtifactTemps
	}
	var bound, sourceWarning bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs j WHERE j.id=? AND j.version=? AND j.status=? AND j.kind=? AND j.resource_key=? AND j.dedupe_token=? AND j.payload_json=? AND EXISTS(SELECT 1 FROM job_resources r WHERE r.job_id=j.id AND r.resource_key=?)),EXISTS(SELECT 1 FROM sources WHERE is_active=1 AND state!='healthy')`, job.ID, job.Version, job.Status, input.Kind, input.ResourceKey, input.Dedupe, string(input.Payload), input.ResourceKey).Scan(&bound, &sourceWarning); err != nil {
		return err
	}
	if !bound {
		return errArtifactTemps
	}
	for _, resource := range resources {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_resources WHERE job_id=? AND resource_key=?)`, job.ID, resource).Scan(&bound); err != nil {
			return err
		}
		if !bound {
			return errArtifactTemps
		}
	}
	if sourceWarning {
		return ErrSourceUnavailable
	}
	return nil
}

func (s *Service) reserveArtifactMaintenance(ctx context.Context, q *jobs.Queue, lease jobs.Lease, input jobs.Input, units int64, resources ...string) (jobs.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return jobs.Job{}, err
	}
	defer tx.Rollback()
	job, err := s.requireArtifactMaintenanceTx(ctx, tx, q, lease, input, resources...)
	if err != nil {
		return jobs.Job{}, err
	}
	if job.TotalKnown || job.Done != job.Total || units < 0 || units > math.MaxInt64-job.Total {
		return jobs.Job{}, jobs.ErrState
	}
	counts := job.Counts
	counts.Total += units
	if _, err := q.ProgressTx(ctx, tx, lease, counts, true); err != nil {
		return jobs.Job{}, err
	}
	if err := q.HeartbeatTx(ctx, tx, lease); err != nil {
		return jobs.Job{}, err
	}
	if _, err := s.requireArtifactMaintenanceTx(ctx, tx, q, lease, input, resources...); err != nil {
		return jobs.Job{}, err
	}
	return job, tx.Commit()
}

type artifactScopeResult struct {
	entries, unlinked, retained, bytes int64
	failed, limited                    bool
}

func (s *Service) recordArtifactMaintenanceScope(ctx context.Context, q *jobs.Queue, lease jobs.Lease, input jobs.Input, scope artifactScopeResult, resources ...string) error {
	scopeEvent, _, err := artifactMaintenanceEvents(input.Kind)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := s.requireArtifactMaintenanceTx(ctx, tx, q, lease, input, resources...)
	if err != nil {
		return err
	}
	if !job.TotalKnown || job.Done >= job.Total {
		return jobs.ErrProgress
	}
	counts := job.Counts
	counts.Done++
	outcome := "skipped"
	switch {
	case scope.failed:
		counts.Failed++
		outcome = "failed"
	case scope.unlinked > 0:
		counts.Succeeded++
		outcome = "succeeded"
	default:
		counts.Skipped++
	}
	if _, err := q.ProgressTx(ctx, tx, lease, counts, true); err != nil {
		return err
	}
	if err := q.HeartbeatTx(ctx, tx, lease); err != nil {
		return err
	}
	detail := map[string]any{"outcome": outcome, "unlinked_files": scope.unlinked, "retained_entries": scope.retained, "limited": scope.limited}
	if input.Kind == "reconcile_artifact" {
		detail["read_bytes"] = scope.bytes
	}
	if err := store.Audit(ctx, tx, "", scopeEvent, "job", job.ID, detail, time.Now().UTC()); err != nil {
		return err
	}
	if _, err := s.requireArtifactMaintenanceTx(ctx, tx, q, lease, input, resources...); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) finishArtifactMaintenance(ctx context.Context, q *jobs.Queue, lease jobs.Lease, input jobs.Input, detail map[string]any, resources ...string) error {
	_, finishedEvent, err := artifactMaintenanceEvents(input.Kind)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := s.requireArtifactMaintenanceTx(ctx, tx, q, lease, input, resources...)
	if err != nil {
		return err
	}
	if err := store.Audit(ctx, tx, "", finishedEvent, "job", job.ID, detail, time.Now().UTC()); err != nil {
		return err
	}
	if _, err := s.requireArtifactMaintenanceTx(ctx, tx, q, lease, input, resources...); err != nil {
		return err
	}
	finished, err := q.CompleteTx(ctx, tx, lease)
	if err != nil {
		return err
	}
	if err := s.requireArtifactMaintenanceStateTx(ctx, tx, finished, input, resources...); err != nil {
		return err
	}
	return tx.Commit()
}
