//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"gallerygo/internal/jobs"

	"golang.org/x/sys/unix"
)

func (s *Service) requireArtifactOrphanTx(ctx context.Context, tx *sql.Tx, q *jobs.Queue, lease jobs.Lease, request artifactOrphanRequest, input jobs.Input) (jobs.Job, error) {
	return s.requireArtifactMaintenanceTx(ctx, tx, q, lease, input, "media:"+request.MediaID, "revision:"+request.RevisionID)
}

// artifactOrphanDecision uses indexed ownership and historical creation-bound
// work, not a hash-looking filename, as provenance. All references are retained.
func (s *Service) artifactOrphanDecision(ctx context.Context, q *jobs.Queue, lease jobs.Lease, request artifactOrphanRequest, input jobs.Input, relative string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	job, err := s.requireArtifactOrphanTx(ctx, tx, q, lease, request, input)
	if err != nil {
		return false, err
	}
	if !job.TotalKnown || job.Done >= job.Total {
		return false, jobs.ErrState
	}
	if err := q.HeartbeatTx(ctx, tx, lease); err != nil {
		return false, err
	}
	if _, err := s.requireArtifactOrphanTx(ctx, tx, q, lease, request, input); err != nil {
		return false, err
	}
	work, _ := stillInputWorkKey(request.MediaID, request.RevisionID, request.Preset)
	var owned, terminal, active, referenced bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM media_revisions r JOIN media m ON m.id=r.media_id JOIN source_entries e ON e.id=m.source_entry_id
 WHERE r.id=? AND r.media_id=? AND e.source_id=? AND m.kind='image' AND m.version>0
 AND typeof(m.version)='integer' AND typeof(r.version)='integer' AND r.version=? AND typeof(r.source_sha256)='text'
 AND r.source_sha256=? AND typeof(r.source_size)='integer' AND r.source_size=? AND typeof(r.source_mtime_ns)='integer' AND r.source_mtime_ns=?),
 EXISTS(SELECT 1 FROM job_resources b JOIN jobs j ON j.id=b.job_id WHERE b.resource_key=? AND j.resource_key=? AND j.dedupe_token=?
 AND j.kind='variant_still' AND j.payload_json='{"version":1}' AND j.status IN('succeeded','failed','cancelled','dead')
 AND EXISTS(SELECT 1 FROM job_resources m WHERE m.job_id=j.id AND m.resource_key=?)
 AND EXISTS(SELECT 1 FROM job_resources s WHERE s.job_id=j.id AND s.resource_key=?)),
 (EXISTS(SELECT 1 FROM jobs WHERE dedupe_key=? AND status IN('queued','running')) OR
 EXISTS(SELECT 1 FROM job_resources b JOIN jobs j ON j.id=b.job_id WHERE b.resource_key=? AND j.status IN('queued','running'))),
 EXISTS(SELECT 1 FROM media_variants WHERE storage_path=?)`, request.RevisionID, request.MediaID, request.SourceID, request.RevisionVersion, request.SourceSHA256, request.SourceSize, request.SourceMTime,
		work, work, work, "media:"+request.MediaID, "source:"+request.SourceID, work, work, relative).Scan(&owned, &terminal, &active, &referenced); err != nil {
		return false, err
	}
	return owned && terminal && !active && !referenced, tx.Commit()
}

// reconcileArtifactOrphans operates on one explicit owned revision/finite-preset
// cohort. It is private/unregistered, never recursive discovery or READY evidence.
func (s *Service) reconcileArtifactOrphans(ctx context.Context, q *jobs.Queue, lease jobs.Lease, request artifactOrphanRequest) (artifactOrphanReport, error) {
	var report artifactOrphanReport
	input, err := s.artifactOrphanInput(request)
	if err != nil {
		return report, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	job, err := s.requireArtifactOrphanTx(ctx, tx, q, lease, request, input)
	rollbackErr := tx.Rollback()
	if err != nil {
		return report, err
	}
	if job.TotalKnown || job.Done != job.Total {
		return report, jobs.ErrState
	}
	if rollbackErr != nil {
		return report, rollbackErr
	}
	gate, err := s.lockArtifacts(ctx, true)
	if err != nil {
		return report, err
	}
	defer gate.Close()
	resources := []string{"media:" + request.MediaID, "revision:" + request.RevisionID}
	if _, err := s.reserveArtifactMaintenance(ctx, q, lease, input, 1, resources...); err != nil {
		return report, err
	}
	allowed, err := s.artifactOrphanDecision(ctx, q, lease, request, input, "")
	if err != nil {
		return report, err
	}
	var scope artifactScopeResult
	if allowed {
		scope, err = s.reconcileArtifactOrphanScope(ctx, q, lease, request, input, gate)
	}
	report.entries, report.bytes, report.unlinked, report.retained, report.limited = scope.entries, scope.bytes, scope.unlinked, scope.retained, scope.limited
	if err != nil {
		return report, err
	}
	if err := gate.check(ctx); err != nil {
		return report, err
	}
	if err := s.recordArtifactMaintenanceScope(ctx, q, lease, input, scope, resources...); err != nil {
		return report, err
	}
	if err := gate.check(ctx); err != nil {
		return report, err
	}
	err = s.finishArtifactMaintenance(ctx, q, lease, input, map[string]any{
		"cohort_scopes": 1, "examined_entries": report.entries, "read_bytes": report.bytes,
		"unlinked_files": report.unlinked, "retained_entries": report.retained, "limited": report.limited,
	}, resources...)
	return report, err
}

type artifactOrphanDirectory struct {
	parent *os.Root
	name   string
	info   fs.FileInfo
	root   *os.Root
}

func reconcileArtifactBytes(ctx context.Context, request artifactOrphanRequest, relative, hash string, data []byte) bool {
	preset, _ := staticPreset(request.Preset)
	policy := DefaultInspectPolicy()
	policy.MaxDimension, policy.MaxPixels = int(preset.longEdge), preset.longEdge*preset.longEdge
	policy.HeaderBytes, policy.MaxFrames = preset.maxBytes, 1
	info, err := inspectWebP(ctx, bytes.NewReader(data), int64(len(data)), policy)
	if err != nil {
		return false
	}
	// Geometry here proves only the finite stored container/key, not that these
	// pixels were decoded/rendered from a source or can be published as READY.
	source := Inspection{Kind: "image", Status: "ok", Width: &info.width, Height: &info.height}
	artifact, err := validateStillArtifact(ctx, request.RevisionID, request.Preset, source, data)
	return err == nil && artifact.storage == relative && artifact.hash == hash
}

func (s *Service) reconcileArtifactOrphanScope(ctx context.Context, q *jobs.Queue, lease jobs.Lease, request artifactOrphanRequest, input jobs.Input, gate *artifactGate) (artifactScopeResult, error) {
	var result artifactScopeResult
	if gate == nil || !gate.exclusive {
		return result, errArtifactTemps
	}
	if err := gate.check(ctx); err != nil {
		return result, err
	}
	parent := gate.root
	directories := make([]artifactOrphanDirectory, 0, 4)
	defer func() {
		for i := len(directories) - 1; i >= 0; i-- {
			directories[i].root.Close()
		}
	}()
	for _, name := range []string{"variants", request.RevisionID, request.Preset, "1"} {
		opened, info, err := openArtifactTempDirectory(parent, name, gate.rootInfo)
		if err != nil {
			result.failed = !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrVariantUnavailable)
			return result, nil
		}
		directories = append(directories, artifactOrphanDirectory{parent, name, info, opened})
		parent = opened
	}
	reader, err := parent.Open(".")
	if err != nil {
		result.failed = true
		return result, nil
	}
	defer reader.Close()
	entries, readErr := reader.ReadDir(request.EntryLimit + 1)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		result.failed = true
		return result, nil
	}
	if len(entries) > request.EntryLimit {
		entries = entries[:request.EntryLimit]
		result.failed, result.limited = true, true
	}
	now := time.Now().UTC()
	preset, _ := staticPreset(request.Preset)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.entries++
		name := entry.Name()
		if len(name) != 69 || !strings.HasSuffix(name, ".webp") || !validHash(name[:64]) {
			result.retained++
			continue
		}
		info, err := parent.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			result.failed = true
			return result, nil
		}
		if !artifactTempPrivate(info, gate.rootInfo, false) || info.Size() < 12 || info.Size() > preset.maxBytes || now.Sub(info.ModTime()) < 24*time.Hour {
			result.retained++
			continue
		}
		relative, err := VariantPath(request.RevisionID, request.Preset, 1, name[:64], "image/webp")
		if err != nil {
			result.retained++
			continue
		}
		allowed, err := s.artifactOrphanDecision(ctx, q, lease, request, input, relative)
		if err != nil {
			return result, err
		}
		if !allowed {
			result.retained++
			continue
		}
		if info.Size() > request.ByteLimit-result.bytes {
			result.failed, result.limited = true, true
			break
		}
		file, opened, err := OpenReadOnlyFile(parent, name)
		if err != nil {
			result.retained++
			continue
		}
		if !os.SameFile(info, opened) || stillFileStamp(info) != stillFileStamp(opened) || !artifactTempPrivate(opened, gate.rootInfo, false) {
			file.Close()
			result.retained++
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(inspectReader{ctx, file}, info.Size()))
		result.bytes += int64(len(data))
		after, statErr := file.Stat()
		file.Close()
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if readErr != nil || statErr != nil {
			result.failed = true
			return result, nil
		}
		if int64(len(data)) != info.Size() || stillFileStamp(info) != stillFileStamp(after) || !reconcileArtifactBytes(ctx, request, relative, name[:64], data) {
			result.retained++
			continue
		}
		allowed, err = s.artifactOrphanDecision(ctx, q, lease, request, input, relative)
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
		for _, directory := range directories {
			current, err := directory.parent.Lstat(directory.name)
			if err != nil || !os.SameFile(current, directory.info) || !artifactTempPrivate(current, gate.rootInfo, true) {
				return result, ErrVariantUnavailable
			}
		}
		named, err := parent.Lstat(name)
		if err != nil || !os.SameFile(info, named) || stillFileStamp(info) != stillFileStamp(named) || !artifactTempPrivate(named, gate.rootInfo, false) {
			result.retained++
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		// Both parent and basename are fixed; flags0 cannot remove a directory.
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
