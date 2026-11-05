package scanner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

const inspectVersion = 9

type inspectedFile struct {
	file       File
	inspection media.Inspection
}

// ProbeMetadata reads source files only outside transactions. Every staged path
// gets an explicit native/unsupported/invalid/non-media result, preserving the
// complete inventory instead of silently dropping failures from reconciliation.
func (s *Inventory) ProbeMetadata(ctx context.Context, lease jobs.Lease, id string, policy media.InspectPolicy) (Generation, error) {
	if err := policy.Validate(); err != nil {
		return Generation{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.policy.Timeout)
	defer cancel()
	policy, err := policy.PinCodecRuntime(ctx)
	if err != nil {
		return Generation{}, err
	}
	generation, configured, err := s.beginMetadata(ctx, lease, id, policy)
	if err != nil {
		return Generation{}, err
	}
	if generation.MetadataComplete {
		return generation, nil
	}
	if err := library.ProbeRoot(ctx, configured); err != nil {
		return Generation{}, err
	}
	root, err := os.OpenRoot(configured.Path)
	if err != nil {
		return Generation{}, err
	}
	defer root.Close()
	hasher := walk{ctx: ctx, root: root, policy: s.policy, buffer: make([]byte, hashChunk)}
	after := ""
	for {
		files, err := s.fileBatch(ctx, id, after, s.policy.BatchSize)
		if err != nil {
			return Generation{}, err
		}
		if len(files) == 0 {
			break
		}
		results := make([]inspectedFile, 0, len(files))
		for _, file := range files {
			inspection, err := inspectStagedFile(ctx, &hasher, file, policy)
			if err != nil {
				return Generation{}, &PathError{file.RelativePath, err}
			}
			results = append(results, inspectedFile{file, inspection})
		}
		generation, err = s.recordMetadata(ctx, lease, id, results)
		if err != nil {
			return Generation{}, err
		}
		after = files[len(files)-1].RelativePath
	}
	if err := library.ProbeRoot(ctx, configured); err != nil {
		return Generation{}, err
	}
	return s.completeMetadata(ctx, lease, id)
}

// Pin the inspector version and limits so a resumed generation cannot mix results
// from different processing policies. A changed policy requires a new generation.
func (s *Inventory) beginMetadata(ctx context.Context, lease jobs.Lease, id string, policy media.InspectPolicy) (Generation, media.SourceRoot, error) {
	if policy.CodecRuntimePin() == "" || policy.ImageRuntimePin() == "" {
		return Generation{}, media.SourceRoot{}, ErrGeneration
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Generation{}, media.SourceRoot{}, err
	}
	defer tx.Rollback()
	generation, _, root, err := s.RequireReadyTx(ctx, tx, lease, id)
	if err != nil {
		return Generation{}, media.SourceRoot{}, err
	}
	data, _ := json.Marshal(struct {
		Version      int
		Policy       media.InspectPolicy
		CodecRuntime string
		ImageRuntime string
	}{inspectVersion, policy, policy.CodecRuntimePin(), policy.ImageRuntimePin()})
	digest := sha256.Sum256(data)
	pin := hex.EncodeToString(digest[:])
	if generation.inspectionPolicy != "" {
		if generation.inspectionPolicy != pin {
			return Generation{}, media.SourceRoot{}, ErrGeneration
		}
		return generation, root, nil
	}
	generation.inspectionPolicy = pin
	if err := s.save(ctx, tx, &generation, s.clock()); err != nil {
		return Generation{}, media.SourceRoot{}, err
	}
	return generation, root, tx.Commit()
}

func inspectStagedFile(ctx context.Context, hasher *walk, file File, policy media.InspectPolicy) (media.Inspection, error) {
	if err := ctx.Err(); err != nil {
		return media.Inspection{}, err
	}
	input, info, err := media.OpenReadOnlyFile(hasher.root, file.RelativePath)
	if err != nil {
		return media.Inspection{}, err
	}
	if !matchesStagedFile(file, info) {
		input.Close()
		return media.Inspection{}, ErrChanged
	}
	inspection, err := media.Inspect(ctx, input, file.RelativePath, policy)
	input.Close()
	if err != nil {
		return media.Inspection{}, err
	}
	// Stat alone cannot bind metadata to a sealed revision: equal-size/mtime
	// overwrites require the same streamed full hash and root-relative path check.
	verified, err := hasher.hashFile(file.RelativePath, info)
	if err != nil {
		return media.Inspection{}, err
	}
	if verified != file {
		return media.Inspection{}, ErrChanged
	}
	return inspection, nil
}

func matchesStagedFile(file File, info os.FileInfo) bool {
	return info.Mode().IsRegular() && file.SizeBytes == info.Size() && file.MtimeNS == info.ModTime().UnixNano() && file.Identity == fileIdentity(info)
}

func (s *Inventory) fileBatch(ctx context.Context, id, after string, limit int) ([]File, error) {
	rows, err := s.db.QueryContext(ctx, fileSelect+` WHERE generation_id=? AND relative_path>? ORDER BY relative_path LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := make([]File, 0, limit)
	for rows.Next() {
		file, err := readFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func validInspection(result media.Inspection) bool {
	if result.Kind != "" && result.Kind != "image" && result.Kind != "video" {
		return false
	}
	if result.Codec != nil && (result.Status != "ok" || !media.ValidCodec(*result.Codec)) {
		return false
	}
	if result.Width != nil && (*result.Width < 1 || result.Height == nil || *result.Height < 1) {
		return false
	}
	if result.Height != nil && result.Width == nil {
		return false
	}
	if result.DurationMS != nil && (result.Status != "ok" || *result.DurationMS < 0) {
		return false
	}
	if result.Animated != nil && result.Status != "ok" {
		return false
	}
	if result.FrameCount != nil && (result.Status != "ok" || *result.FrameCount < 1 || result.Animated == nil || (*result.FrameCount > 1) != *result.Animated) {
		return false
	}
	if result.LoopCount != nil && (result.Status != "ok" || *result.LoopCount < 0 || result.FrameCount == nil) {
		return false
	}
	if result.Orientation != nil && (result.Status != "ok" || *result.Orientation < 1 || *result.Orientation > 8) {
		return false
	}
	if result.CaptureAt != nil {
		_, offset := result.CaptureAt.Zone()
		if result.Status != "ok" || offset != 0 || result.CaptureAt.Year() < 1 || result.CaptureAt.Year() > 9999 || result.CaptureAt.Nanosecond()%1000000 != 0 {
			return false
		}
	}
	switch result.Status {
	case "ok":
		return result.Kind != "" && result.MIME != "application/octet-stream" && result.Width != nil && result.Codec != nil && result.ErrorCode == nil
	case "unsupported":
		return result.Kind != "" && result.ErrorCode != nil && *result.ErrorCode == "CODEC_UNAVAILABLE" && result.Width == nil && result.Height == nil
	case "invalid":
		return result.Kind != "" && result.ErrorCode != nil && (*result.ErrorCode == "INVALID_MEDIA" || *result.ErrorCode == "MEDIA_LIMIT") && result.Width == nil && result.Height == nil
	case "not_media":
		return result.Kind == "" && result.MIME == "application/octet-stream" && result.ErrorCode == nil && result.Width == nil && result.Height == nil
	default:
		return false
	}
}

func (s *Inventory) recordMetadata(ctx context.Context, lease jobs.Lease, id string, results []inspectedFile) (Generation, error) {
	if len(results) < 1 || len(results) > s.policy.BatchSize {
		return Generation{}, ErrLimit
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Generation{}, err
	}
	defer tx.Rollback()
	generation, _, _, err := s.RequireReadyTx(ctx, tx, lease, id)
	if err != nil {
		return Generation{}, err
	}
	if !validDigest(generation.inspectionPolicy) {
		return Generation{}, ErrGeneration
	}
	job, err := s.queue.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return Generation{}, err
	}
	var added, succeeded, failed, skipped int64
	now := s.clock()
	for _, result := range results {
		if !validInspection(result.inspection) {
			return Generation{}, ErrUnsafeEntry
		}
		expected, err := readFile(tx.QueryRowContext(ctx, fileSelect+` WHERE generation_id=? AND relative_path=?`, id, result.file.RelativePath))
		if err != nil {
			return Generation{}, err
		}
		if expected != result.file {
			return Generation{}, ErrChanged
		}
		data, err := json.Marshal(result.inspection)
		if err != nil {
			return Generation{}, err
		}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO inventory_media(generation_id,relative_path,inspect_version,status,kind,mime,inspection_json) VALUES(?,?,?,?,?,?,?) ON CONFLICT(generation_id,relative_path) DO NOTHING`, id, result.file.RelativePath, inspectVersion, result.inspection.Status, result.inspection.Kind, result.inspection.MIME, string(data))
		if err != nil {
			return Generation{}, err
		}
		count, err := inserted.RowsAffected()
		if err != nil {
			return Generation{}, err
		}
		if count == 0 {
			var previous string
			var version int
			if err := tx.QueryRowContext(ctx, `SELECT inspect_version,inspection_json FROM inventory_media WHERE generation_id=? AND relative_path=?`, id, result.file.RelativePath).Scan(&version, &previous); err != nil {
				return Generation{}, err
			}
			if version != inspectVersion || previous != string(data) {
				return Generation{}, ErrChanged
			}
			continue
		}
		if generation.MetadataComplete {
			return Generation{}, ErrGeneration
		}
		added++
		switch result.inspection.Status {
		case "ok":
			succeeded++
		case "not_media":
			skipped++
		default:
			failed++
		}
		if result.inspection.ErrorCode != nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO inventory_issues(id,generation_id,relative_path,error_code,created_at) VALUES(?,?,?,?,?) ON CONFLICT(generation_id,relative_path,error_code) DO NOTHING`, store.NewID(), id, result.file.RelativePath, *result.inspection.ErrorCode, now.UnixMilli()); err != nil {
				return Generation{}, err
			}
		}
	}
	if added == 0 {
		return generation, nil
	}
	if added > generation.FileCount-generation.MetadataCount {
		return Generation{}, ErrGeneration
	}
	counts := job.Counts
	if added > math.MaxInt64-counts.Total || added > math.MaxInt64-counts.Done || succeeded > math.MaxInt64-counts.Succeeded || failed > math.MaxInt64-counts.Failed || skipped > math.MaxInt64-counts.Skipped {
		return Generation{}, ErrLimit
	}
	counts.Total += added
	counts.Done += added
	counts.Succeeded += succeeded
	counts.Failed += failed
	counts.Skipped += skipped
	generation.MetadataCount += added
	if err := s.save(ctx, tx, &generation, now); err != nil {
		return Generation{}, err
	}
	if _, err := s.queue.ProgressTx(ctx, tx, lease, counts, false); err != nil {
		return Generation{}, err
	}
	return generation, tx.Commit()
}

func (s *Inventory) completeMetadata(ctx context.Context, lease jobs.Lease, id string) (Generation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Generation{}, err
	}
	defer tx.Rollback()
	generation, _, _, err := s.RequireReadyTx(ctx, tx, lease, id)
	if err != nil {
		return Generation{}, err
	}
	if generation.MetadataComplete {
		return generation, nil
	}
	if !validDigest(generation.inspectionPolicy) {
		return Generation{}, ErrGeneration
	}
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inventory_media WHERE generation_id=?`, id).Scan(&count); err != nil {
		return Generation{}, err
	}
	if count != generation.FileCount || count != generation.MetadataCount {
		return Generation{}, ErrGeneration
	}
	generation.MetadataComplete = true
	now := s.clock()
	if err := s.save(ctx, tx, &generation, now); err != nil {
		return Generation{}, err
	}
	if err := store.Audit(ctx, tx, "", "inventory_metadata_staged", "inventory", id, map[string]any{"source_id": generation.SourceID, "files": generation.FileCount}, now); err != nil {
		return Generation{}, err
	}
	return generation, tx.Commit()
}

// RequireMetadataTx adds complete derived-metadata evidence to the live ready
// generation/config/lease publisher boundary. It never performs filesystem I/O.
func (s *Inventory) RequireMetadataTx(ctx context.Context, tx *sql.Tx, lease jobs.Lease, id string) (Generation, library.Source, media.SourceRoot, error) {
	generation, source, root, err := s.RequireReadyTx(ctx, tx, lease, id)
	if err != nil {
		return Generation{}, library.Source{}, media.SourceRoot{}, err
	}
	if !generation.MetadataComplete || generation.MetadataCount != generation.FileCount || !validDigest(generation.inspectionPolicy) {
		return Generation{}, library.Source{}, media.SourceRoot{}, ErrGeneration
	}
	return generation, source, root, nil
}

func readInspection(row interface{ Scan(...any) error }) (media.Inspection, error) {
	var data string
	if err := row.Scan(&data); err != nil {
		return media.Inspection{}, err
	}
	var result media.Inspection
	if err := json.Unmarshal([]byte(data), &result); err != nil {
		return media.Inspection{}, err
	}
	if !validInspection(result) {
		return media.Inspection{}, errors.New("invalid stored media inspection")
	}
	return result, nil
}
