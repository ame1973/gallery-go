package media

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"time"

	"gallerygo/internal/jobs"
)

var errStillInput = errors.New("static input no longer matches its leased revision")

// stillInputSnapshot contains private indexed identities, never an API projection.
// Reachability alone cannot satisfy entry/config/revision/hash equality.
type stillInputSnapshot struct {
	mediaID, albumID, libraryID, entryID, sourceID, sourceName             string
	rootPath, rootSignature, relative, fileIdentity, hash, metadata        string
	mediaVersion, entryVersion, sourceConfig, revisionVersion, size, mtime int64
	revision                                                               string
}

func (snap stillInputSnapshot) String() string {
	return "private static input for media " + snap.mediaID
}

type stillInputFile struct {
	root                           *os.Root
	file                           *os.File
	rootInfo, markerInfo, fileInfo fs.FileInfo
	stamp                          string
}

func (input *stillInputFile) close() error {
	if input == nil {
		return nil
	}
	var fileErr, rootErr error
	if input.file != nil {
		fileErr = input.file.Close()
		input.file = nil
	}
	if input.root != nil {
		rootErr = input.root.Close()
		input.root = nil
	}
	return errors.Join(fileErr, rootErr)
}

// preparedStillInput is a read-only source capsule, not decoded/rendered evidence
// or permission to publish READY. A producer must rehash after its own work and
// recheck these DB pins/lease in its publication transaction. No public constructor.
type preparedStillInput struct {
	snapshot             stillInputSnapshot
	input                *stillInputFile
	jobID, owner, preset string
	attempt              int
	maxBytes             int64
	inspection           Inspection
}

func (input *preparedStillInput) String() string {
	return "prepared static input for media " + input.snapshot.mediaID
}
func (input *preparedStillInput) Close() error {
	if input == nil {
		return nil
	}
	return input.input.close()
}

func stillInputWorkKey(mediaID, revision, key string) (string, error) {
	preset, err := staticPreset(key)
	if err != nil {
		return "", errStillInput
	}
	// VariantPath supplies the existing finite preset/revision/path grammar.
	if !localPath(mediaID) || strings.ContainsAny(mediaID, "/\x00\r\n") || len(mediaID) > 64 || len(revision) > 128 || strings.ContainsAny(revision, "\x00\r\n") {
		return "", errStillInput
	}
	if _, err := VariantPath(revision, preset.key, preset.version, strings.Repeat("0", 64), "image/webp"); err != nil {
		return "", errStillInput
	}
	data, _ := json.Marshal(struct {
		Media, Revision, Preset string
		Version                 int64
	}{mediaID, revision, preset.key, preset.version})
	digest := sha256.Sum256(data)
	return "variant_still:" + hex.EncodeToString(digest[:]), nil
}

func (s *Service) stillInputInTx(ctx context.Context, tx *sql.Tx, q *jobs.Queue, lease jobs.Lease, mediaID, revision, key string, maxBytes int64) (stillInputSnapshot, Inspection, error) {
	if maxBytes < 1 || maxBytes > 1<<40 {
		return stillInputSnapshot{}, Inspection{}, errStillInput
	}
	identity, err := stillInputWorkKey(mediaID, revision, key)
	if err != nil {
		return stillInputSnapshot{}, Inspection{}, err
	}
	job, err := q.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return stillInputSnapshot{}, Inspection{}, err
	}
	if job.Kind != "variant_still" || job.ResourceKey != identity || job.Dedupe != identity || string(job.Payload) != `{"version":1}` {
		return stillInputSnapshot{}, Inspection{}, errStillInput
	}
	var snap stillInputSnapshot
	err = tx.QueryRowContext(ctx, `SELECT m.id,m.album_id,a.library_id,e.id,src.id,src.name,src.root_path,src.root_config_sha256,e.relative_path,e.file_identity,COALESCE(e.sha256,''),m.metadata_json,m.version,e.version,src.config_version,r.version,r.source_size,r.source_mtime_ns,r.id
 FROM media m JOIN albums a ON a.id=m.album_id JOIN libraries l ON l.id=a.library_id
 JOIN source_entries e ON e.id=m.source_entry_id AND e.source_id=a.source_id
 JOIN sources src ON src.id=e.source_id AND src.library_id=l.id
 JOIN media_revisions r ON r.id=m.current_revision_id AND r.media_id=m.id
 WHERE m.id=? AND r.id=? AND m.kind='image' AND m.availability='available' AND e.availability='present'
 AND src.is_active=1 AND l.is_active=1 AND src.state='healthy' AND e.source_config_version=src.config_version
 AND r.source_sha256=e.sha256 AND r.source_size=e.size_bytes AND r.source_mtime_ns=e.mtime_ns`, mediaID, revision).Scan(&snap.mediaID, &snap.albumID, &snap.libraryID, &snap.entryID, &snap.sourceID, &snap.sourceName, &snap.rootPath, &snap.rootSignature, &snap.relative, &snap.fileIdentity, &snap.hash, &snap.metadata, &snap.mediaVersion, &snap.entryVersion, &snap.sourceConfig, &snap.revisionVersion, &snap.size, &snap.mtime, &snap.revision)
	if err != nil {
		return stillInputSnapshot{}, Inspection{}, err
	}
	root, ok := s.sources[snap.sourceName]
	if !ok || root.Path != snap.rootPath || root.Signature() != snap.rootSignature || !localPath(root.Marker) || !localPath(snap.relative) || len(snap.relative) > 4096 || snap.fileIdentity == "" || !validHash(snap.hash) || snap.size < 1 || snap.size > maxBytes || snap.mediaVersion < 1 || snap.entryVersion < 1 || snap.sourceConfig < 1 || snap.revisionVersion < 1 {
		return stillInputSnapshot{}, Inspection{}, errStillInput
	}
	var bound bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_resources WHERE job_id=? AND resource_key=?) AND EXISTS(SELECT 1 FROM job_resources WHERE job_id=? AND resource_key=?)`, job.ID, "media:"+snap.mediaID, job.ID, "source:"+snap.sourceID).Scan(&bound); err != nil {
		return stillInputSnapshot{}, Inspection{}, err
	}
	if !bound {
		return stillInputSnapshot{}, Inspection{}, errStillInput
	}
	var inspection Inspection
	if err := json.Unmarshal([]byte(snap.metadata), &inspection); err != nil {
		return stillInputSnapshot{}, Inspection{}, errStillInput
	}
	preset, _ := staticPreset(key)
	if _, _, err := preset.dimensions(inspection); err != nil {
		return stillInputSnapshot{}, Inspection{}, errStillInput
	}
	return snap, inspection, nil
}

// prepareStillInput performs all source IO outside a writer transaction. Its only
// accepted work identity is the exact private media/revision/preset lease plus
// original creation-bound media/source dependencies; it is not an HTTP download.
func (s *Service) prepareStillInput(ctx context.Context, q *jobs.Queue, lease jobs.Lease, mediaID, revision, key string, maxBytes int64) (*preparedStillInput, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	snapshot, inspection, err := s.stillInputInTx(ctx, tx, q, lease, mediaID, revision, key, maxBytes)
	rollbackErr := tx.Rollback()
	if err != nil {
		return nil, err
	}
	if rollbackErr != nil {
		return nil, rollbackErr
	}
	file, err := openStillInputFile(ctx, snapshot, s.sources[snapshot.sourceName], maxBytes)
	if err != nil {
		return nil, err
	}
	prepared := &preparedStillInput{snapshot: snapshot, input: file, jobID: lease.Job.ID, owner: lease.Owner, preset: key, attempt: lease.Job.Attempts, maxBytes: maxBytes, inspection: inspection}
	tx, err = s.db.BeginTx(ctx, nil)
	if err == nil {
		err = prepared.requireTx(ctx, s, tx, q, lease)
		rollbackErr = tx.Rollback()
		if err == nil {
			err = rollbackErr
		}
	}
	if err != nil {
		prepared.Close()
		return nil, err
	}
	return prepared, nil
}

// recheck hashes the same pinned read-only descriptor and its current rooted name
// again after producer work. This is still not an atomic NAS snapshot or decoding.
func (input *preparedStillInput) recheck(ctx context.Context, s *Service, q *jobs.Queue, lease jobs.Lease) error {
	if input == nil || input.input == nil || input.input.file == nil {
		return errStillInput
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := verifyStillInputFile(ctx, input.input, input.snapshot, s.sources[input.snapshot.sourceName], input.maxBytes); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return input.requireTx(ctx, s, tx, q, lease)
}

// requireTx is DB-only and must share a future publication's business transaction.
// It deliberately cannot turn this source capsule into rendered/READY evidence.
func (input *preparedStillInput) requireTx(ctx context.Context, s *Service, tx *sql.Tx, q *jobs.Queue, lease jobs.Lease) error {
	if input == nil || input.input == nil || input.input.file == nil || input.jobID != lease.Job.ID || input.owner != lease.Owner || input.attempt != lease.Job.Attempts {
		return errStillInput
	}
	current, _, err := s.stillInputInTx(ctx, tx, q, lease, input.snapshot.mediaID, input.snapshot.revision, input.preset, input.maxBytes)
	if err != nil {
		return err
	}
	if current != input.snapshot {
		return errStillInput
	}
	return nil
}
