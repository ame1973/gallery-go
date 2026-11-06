//go:build linux && (amd64 || arm64)

package media

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"gallerygo/internal/jobs"
	"gallerygo/internal/store"

	"golang.org/x/sys/unix"
)

type artifactTempOwner struct {
	id, kind, status, resource, dedupe, payload string
	version                                     int64
	bound                                       bool
}

const artifactTempOwners = `SELECT j.id,j.kind,j.status,j.version,COALESCE(j.resource_key,''),j.dedupe_token,j.payload_json,
 CASE WHEN j.kind='variant_still' THEN
 EXISTS(SELECT 1 FROM job_resources r WHERE r.job_id=j.id AND r.resource_key GLOB 'media:?*') AND
 EXISTS(SELECT 1 FROM job_resources r WHERE r.job_id=j.id AND r.resource_key GLOB 'source:?*') ELSE 0 END
 FROM jobs j`

func scanArtifactTempOwner(row interface{ Scan(...any) error }) (artifactTempOwner, error) {
	var owner artifactTempOwner
	err := row.Scan(&owner.id, &owner.kind, &owner.status, &owner.version, &owner.resource, &owner.dedupe, &owner.payload, &owner.bound)
	return owner, err
}

func (owner artifactTempOwner) eligible() bool {
	terminal := owner.status == "succeeded" || owner.status == "failed" || owner.status == "cancelled" || owner.status == "dead"
	return terminal && artifactTempJobID(owner.id) && owner.kind == "variant_still" && owner.version > 0 && owner.bound && owner.payload == `{"version":1}` && owner.resource == owner.dedupe && strings.HasPrefix(owner.resource, "variant_still:") && validHash(strings.TrimPrefix(owner.resource, "variant_still:"))
}

func (s *Service) requireArtifactTempsTx(ctx context.Context, tx *sql.Tx, q *jobs.Queue, lease jobs.Lease, request artifactTempRequest) (jobs.Job, error) {
	input, err := s.artifactTempInput(request)
	if err != nil {
		return jobs.Job{}, err
	}
	job, err := q.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return jobs.Job{}, err
	}
	if job.Kind != input.Kind || job.ResourceKey != input.ResourceKey || job.Dedupe != input.Dedupe || string(job.Payload) != string(input.Payload) {
		return jobs.Job{}, errArtifactTemps
	}
	var bound bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_resources WHERE job_id=? AND resource_key=?)`, job.ID, input.ResourceKey).Scan(&bound); err != nil {
		return jobs.Job{}, err
	}
	if !bound {
		return jobs.Job{}, errArtifactTemps
	}
	var sourceWarning bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sources WHERE is_active=1 AND state!='healthy')`).Scan(&sourceWarning); err != nil {
		return jobs.Job{}, err
	}
	if sourceWarning {
		return jobs.Job{}, ErrSourceUnavailable
	}
	return job, nil
}

// reconcileArtifactTemps is an unregistered private bounded batch. One selected DB
// job scope is a work unit, not one deleted file or a complete cache sweep. The
// exclusive gate spans decisions and IO; no SQLite writer spans filesystem work.
func (s *Service) reconcileArtifactTemps(ctx context.Context, q *jobs.Queue, lease jobs.Lease, request artifactTempRequest) (artifactTempReport, error) {
	report := artifactTempReport{nextJobID: request.AfterJobID}
	if _, err := s.artifactTempInput(request); err != nil {
		return report, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	job, err := s.requireArtifactTempsTx(ctx, tx, q, lease, request)
	if err != nil || job.TotalKnown || job.Done != job.Total {
		tx.Rollback()
		if err == nil {
			err = jobs.ErrState
		}
		return report, err
	}
	rows, err := tx.QueryContext(ctx, artifactTempOwners+` WHERE j.id>? AND j.id<? ORDER BY j.id LIMIT ?`, request.AfterJobID, job.ID, request.JobLimit+1)
	if err != nil {
		tx.Rollback()
		return report, err
	}
	owners := make([]artifactTempOwner, 0, request.JobLimit+1)
	for rows.Next() {
		owner, err := scanArtifactTempOwner(rows)
		if err != nil {
			rows.Close()
			tx.Rollback()
			return report, err
		}
		owners = append(owners, owner)
	}
	err = rows.Err()
	rows.Close()
	rollbackErr := tx.Rollback()
	if err != nil {
		return report, err
	}
	if rollbackErr != nil {
		return report, rollbackErr
	}
	if len(owners) > request.JobLimit {
		report.hasMore = true
		owners = owners[:request.JobLimit]
	}
	gate, err := s.lockArtifacts(ctx, true)
	if err != nil {
		return report, err
	}
	defer gate.Close()
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	job, err = s.requireArtifactTempsTx(ctx, tx, q, lease, request)
	if err == nil && (job.TotalKnown || job.Done != job.Total || int64(len(owners)) > math.MaxInt64-job.Total) {
		err = jobs.ErrState
	}
	if err == nil {
		counts := job.Counts
		counts.Total += int64(len(owners))
		_, err = q.ProgressTx(ctx, tx, lease, counts, true)
	}
	if err == nil {
		err = q.HeartbeatTx(ctx, tx, lease)
	}
	if err == nil {
		_, err = s.requireArtifactTempsTx(ctx, tx, q, lease, request)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err != nil {
		return report, err
	}
	now := time.Now().UTC()
	for _, owner := range owners {
		var scope artifactTempScope
		if report.limited {
			scope.failed, scope.limited = true, true
		} else if owner.eligible() {
			scope, err = s.reconcileArtifactTempScope(ctx, q, lease, request, gate, owner, request.EntryLimit-int(report.entries), now)
		}
		report.entries += scope.entries
		report.unlinked += scope.unlinked
		report.retained += scope.retained
		if err != nil {
			return report, err
		}
		if scope.limited {
			report.limited, report.hasMore = true, true
		}
		if err := gate.check(ctx); err != nil {
			return report, err
		}
		if err := s.recordArtifactTempScope(ctx, q, lease, request, scope); err != nil {
			return report, err
		}
		report.scopes++
		if !report.limited {
			report.nextJobID = owner.id
		}
	}
	if err := gate.check(ctx); err != nil {
		return report, err
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	if _, err := s.requireArtifactTempsTx(ctx, tx, q, lease, request); err != nil {
		return report, err
	}
	if err := store.Audit(ctx, tx, "", "artifact_temps_reconciled", "job", job.ID, map[string]any{
		"job_scopes": report.scopes, "examined_entries": report.entries, "unlinked_files": report.unlinked,
		"retained_entries": report.retained, "limited": report.limited, "has_more": report.hasMore, "next_job_id": report.nextJobID,
	}, time.Now().UTC()); err != nil {
		return report, err
	}
	if _, err := s.requireArtifactTempsTx(ctx, tx, q, lease, request); err != nil {
		return report, err
	}
	if _, err := q.CompleteTx(ctx, tx, lease); err != nil {
		return report, err
	}
	return report, tx.Commit()
}

type artifactTempScope struct {
	entries, unlinked, retained int64
	failed, limited             bool
}

func (s *Service) recordArtifactTempScope(ctx context.Context, q *jobs.Queue, lease jobs.Lease, request artifactTempRequest, scope artifactTempScope) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := s.requireArtifactTempsTx(ctx, tx, q, lease, request)
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
	if err := store.Audit(ctx, tx, "", "artifact_temp_scope_checked", "job", job.ID, map[string]any{
		"outcome": outcome, "unlinked_files": scope.unlinked, "retained_entries": scope.retained, "limited": scope.limited,
	}, time.Now().UTC()); err != nil {
		return err
	}
	if _, err := s.requireArtifactTempsTx(ctx, tx, q, lease, request); err != nil {
		return err
	}
	return tx.Commit()
}

// A job retry may queue while the gate is held, but cannot reopen a temp inode:
// every cooperating writer first needs the shared gate and creates a new UUID.
func (s *Service) artifactTempDecision(ctx context.Context, q *jobs.Queue, lease jobs.Lease, request artifactTempRequest, owner artifactTempOwner, relative string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	job, err := s.requireArtifactTempsTx(ctx, tx, q, lease, request)
	if err != nil {
		return false, err
	}
	if !job.TotalKnown || job.Done >= job.Total {
		return false, jobs.ErrState
	}
	current, err := scanArtifactTempOwner(tx.QueryRowContext(ctx, artifactTempOwners+` WHERE j.id=?`, owner.id))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	allowed := err == nil && current == owner && current.eligible()
	var referenced bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM media_variants WHERE storage_path=?)`, relative).Scan(&referenced); err != nil {
		return false, err
	}
	if err := q.HeartbeatTx(ctx, tx, lease); err != nil {
		return false, err
	}
	if _, err := s.requireArtifactTempsTx(ctx, tx, q, lease, request); err != nil {
		return false, err
	}
	return allowed && !referenced, tx.Commit()
}

func artifactTempPrivate(info, cache fs.FileInfo, directory bool) bool {
	if info == nil || cache == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	root, rootOK := cache.Sys().(*syscall.Stat_t)
	if !ok || !rootOK || stat.Dev != root.Dev || stat.Uid != uint32(os.Geteuid()) || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	if directory {
		return info.IsDir() && info.Mode().Perm() == 0o700
	}
	return info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && stat.Nlink == 1
}

func openArtifactTempDirectory(parent *os.Root, name string, cache fs.FileInfo) (*os.Root, fs.FileInfo, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !artifactTempPrivate(info, cache, true) {
		return nil, nil, ErrVariantUnavailable
	}
	opened, err := parent.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	current, err := opened.Stat(".")
	if err != nil || !os.SameFile(info, current) || !artifactTempPrivate(current, cache, true) {
		opened.Close()
		return nil, nil, ErrVariantUnavailable
	}
	return opened, info, nil
}

func (s *Service) reconcileArtifactTempScope(ctx context.Context, q *jobs.Queue, lease jobs.Lease, request artifactTempRequest, gate *artifactGate, owner artifactTempOwner, remaining int, now time.Time) (artifactTempScope, error) {
	var result artifactTempScope
	if gate == nil || !gate.exclusive {
		return result, errArtifactTemps
	}
	if remaining < 1 {
		result.failed, result.limited = true, true
		return result, nil
	}
	if err := gate.check(ctx); err != nil {
		return result, err
	}
	temporary, tempInfo, err := openArtifactTempDirectory(gate.root, "tmp", gate.rootInfo)
	if err != nil {
		result.failed = !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrVariantUnavailable)
		return result, nil
	}
	defer temporary.Close()
	directory, dirInfo, err := openArtifactTempDirectory(temporary, owner.id, gate.rootInfo)
	if err != nil {
		result.failed = !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrVariantUnavailable)
		return result, nil
	}
	defer directory.Close()
	reader, err := directory.Open(".")
	if err != nil {
		result.failed = true
		return result, nil
	}
	defer reader.Close()
	entries, readErr := reader.ReadDir(remaining + 1)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		result.failed = true
		return result, nil
	}
	if len(entries) > remaining {
		entries = entries[:remaining]
		result.failed, result.limited = true, true
	}
	maximum, _ := staticPreset("display_1920")
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.entries++
		name := entry.Name()
		if !artifactTempName.MatchString(name) {
			result.retained++
			continue
		}
		info, err := directory.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			result.failed = true
			return result, nil
		}
		if !artifactTempPrivate(info, gate.rootInfo, false) || info.Size() < 0 || info.Size() > maximum.maxBytes || now.Sub(info.ModTime()) < 24*time.Hour {
			result.retained++
			continue
		}
		allowed, err := s.artifactTempDecision(ctx, q, lease, request, owner, path.Join("tmp", owner.id, name))
		if err != nil {
			return result, err
		}
		if !allowed {
			result.retained++
			continue
		}
		if err := gate.check(ctx); err != nil {
			return result, err
		}
		namedTemp, tempErr := gate.root.Lstat("tmp")
		namedJob, jobErr := temporary.Lstat(owner.id)
		if tempErr != nil || jobErr != nil || !os.SameFile(namedTemp, tempInfo) || !os.SameFile(namedJob, dirInfo) || !artifactTempPrivate(namedTemp, gate.rootInfo, true) || !artifactTempPrivate(namedJob, gate.rootInfo, true) {
			return result, ErrVariantUnavailable
		}
		file, current, err := OpenReadOnlyFile(directory, name)
		if err != nil {
			result.retained++
			continue
		}
		stable := os.SameFile(info, current) && stillFileStamp(info) == stillFileStamp(current) && artifactTempPrivate(current, gate.rootInfo, false)
		file.Close()
		if !stable {
			result.retained++
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		// The pinned parent and validated basename cannot traverse out; flags=0 also
		// makes the kernel refuse directory removal, even if a name is replaced.
		if err := unix.Unlinkat(int(reader.Fd()), name, 0); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			result.failed = true
			return result, nil
		}
		result.unlinked++
		if err := reader.Sync(); err != nil {
			result.failed = true
			return result, nil
		}
	}
	return result, nil
}
