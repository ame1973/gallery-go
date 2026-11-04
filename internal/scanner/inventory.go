package scanner

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

var (
	ErrGeneration       = errors.New("inventory generation is no longer active or owned by this attempt")
	ErrActiveGeneration = errors.New("another live inventory generation owns this source")
	ErrMinimumFiles     = errors.New("completed inventory did not meet the minimum-file guard")
	identityPattern     = regexp.MustCompile(`^[0-9a-f]+:[0-9a-f]+$`)
)

type Generation struct {
	ID               string     `json:"id"`
	SourceID         string     `json:"source_id"`
	JobID            string     `json:"job_id"`
	Attempt          int        `json:"attempt"`
	ConfigVersion    int64      `json:"config_version"`
	Number           int64      `json:"generation"`
	Mode             string     `json:"mode"`
	State            string     `json:"state"`
	Healthy          bool       `json:"healthy"`
	FileCount        int64      `json:"file_count"`
	MetadataCount    int64      `json:"metadata_count"`
	MetadataComplete bool       `json:"metadata_complete"`
	SizeBytes        int64      `json:"size_bytes"`
	ErrorCode        *string    `json:"error_code"`
	Version          int64      `json:"version"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	CompletedAt      *time.Time `json:"completed_at"`
	PublishedAt      *time.Time `json:"published_at"`
	owner            string
	scope            string
	base             jobs.Counts
	inspectionPolicy string
}

func (generation Generation) String() string {
	return "inventory " + generation.ID + " (" + generation.State + ")"
}

const generationSelect = `SELECT id,source_id,job_id,attempt,config_version,generation,mode,state,healthy,file_count,size_bytes,error_code,version,created_at,updated_at,completed_at,published_at,lease_owner,scope_path,base_counts_json,metadata_count,metadata_complete,inspect_policy_sha256 FROM inventory_generations`

func scanGeneration(row interface{ Scan(...any) error }) (Generation, error) {
	var generation Generation
	var created, updated int64
	var completed, published sql.NullInt64
	var counts string
	err := row.Scan(&generation.ID, &generation.SourceID, &generation.JobID, &generation.Attempt, &generation.ConfigVersion, &generation.Number, &generation.Mode, &generation.State, &generation.Healthy, &generation.FileCount, &generation.SizeBytes, &generation.ErrorCode, &generation.Version, &created, &updated, &completed, &published, &generation.owner, &generation.scope, &counts, &generation.MetadataCount, &generation.MetadataComplete, &generation.inspectionPolicy)
	if err != nil {
		return Generation{}, err
	}
	if err := json.Unmarshal([]byte(counts), &generation.base); err != nil {
		return Generation{}, err
	}
	generation.CreatedAt, generation.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	if completed.Valid {
		instant := time.UnixMilli(completed.Int64).UTC()
		generation.CompletedAt = &instant
	}
	if published.Valid {
		instant := time.UnixMilli(published.Int64).UTC()
		generation.PublishedAt = &instant
	}
	return generation, nil
}

type Inventory struct {
	db      *sql.DB
	queue   *jobs.Queue
	sources *library.Sources
	policy  Policy
	now     func() time.Time
}

func NewInventory(db *sql.DB, queue *jobs.Queue, sources *library.Sources, policy Policy) (*Inventory, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &Inventory{db: db, queue: queue, sources: sources, policy: policy, now: time.Now}, nil
}

func (s *Inventory) clock() time.Time { return s.now().UTC().Truncate(time.Millisecond) }

func (s *Inventory) Get(ctx context.Context, id string) (Generation, error) {
	return scanGeneration(s.db.QueryRowContext(ctx, generationSelect+` WHERE id=?`, id))
}

// Begin runs after job claim. A live source has one active generation, even when
// differently deduplicated jobs compete. Dead owners are superseded transactionally;
// merely holding an old generation ID never gives an abandoned worker authority.
func (s *Inventory) Begin(ctx context.Context, lease jobs.Lease, sourceID string, configVersion int64, mode, scope string) (Generation, error) {
	if (mode != "full" && mode != "incremental") || (scope != "." && !validRelative(scope)) {
		return Generation{}, ErrGeneration
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Generation{}, err
	}
	defer tx.Rollback()
	job, err := s.queue.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return Generation{}, err
	}
	source, _, err := s.sources.RequireConfigTx(ctx, tx, sourceID, configVersion)
	if err != nil {
		return Generation{}, err
	}
	current, err := scanGeneration(tx.QueryRowContext(ctx, generationSelect+` WHERE job_id=? AND attempt=? AND source_id=?`, job.ID, job.Attempts, sourceID))
	if err == nil {
		if current.owner != lease.Owner || current.ConfigVersion != configVersion || current.Mode != mode || current.scope != scope || (current.State != "walking" && current.State != "ready") {
			return Generation{}, ErrGeneration
		}
		return current, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Generation{}, err
	}
	active, err := scanGeneration(tx.QueryRowContext(ctx, generationSelect+` WHERE source_id=? AND state IN ('walking','ready')`, sourceID))
	now := s.clock()
	if err == nil {
		var live bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE id=? AND attempts=? AND status='running' AND cancel_requested=0 AND lease_owner=? AND lease_expires_at>?)`, active.JobID, active.Attempt, active.owner, now.UnixMilli()).Scan(&live); err != nil {
			return Generation{}, err
		}
		if live && active.ConfigVersion == configVersion {
			return Generation{}, ErrActiveGeneration
		}
		active.State, active.Healthy, active.CompletedAt = "superseded", false, &now
		code := "SOURCE_CHANGED"
		if active.ConfigVersion == configVersion {
			code = "LEASE_EXPIRED"
		}
		active.ErrorCode = &code
		if err := s.save(ctx, tx, &active, now); err != nil {
			return Generation{}, err
		}
		if err := store.Audit(ctx, tx, "", "inventory_superseded", "inventory", active.ID, map[string]any{"source_id": sourceID, "attempt": active.Attempt}, now); err != nil {
			return Generation{}, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Generation{}, err
	}
	var number int64
	if err := tx.QueryRowContext(ctx, `SELECT max(?,COALESCE(MAX(generation),0)) FROM inventory_generations WHERE source_id=?`, source.ScanGeneration, sourceID).Scan(&number); err != nil {
		return Generation{}, err
	}
	if number == math.MaxInt64 {
		return Generation{}, ErrLimit
	}
	generation := Generation{ID: store.NewID(), SourceID: sourceID, JobID: job.ID, Attempt: job.Attempts, ConfigVersion: configVersion, Number: number + 1, Mode: mode, State: "walking", Version: 1, CreatedAt: now, UpdatedAt: now, owner: lease.Owner, scope: scope, base: job.Counts}
	counts, _ := json.Marshal(generation.base)
	if _, err := tx.ExecContext(ctx, `INSERT INTO inventory_generations(id,source_id,job_id,attempt,lease_owner,config_version,generation,mode,scope_path,state,base_counts_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'walking',?,?,?)`, generation.ID, sourceID, job.ID, job.Attempts, lease.Owner, configVersion, generation.Number, mode, scope, string(counts), now.UnixMilli(), now.UnixMilli()); err != nil {
		return Generation{}, err
	}
	if err := store.Change(ctx, tx, "system", "inventory", generation.ID, "upsert", generation.Version, generation, now); err != nil {
		return Generation{}, err
	}
	if err := store.Audit(ctx, tx, "", "inventory_started", "inventory", generation.ID, map[string]any{"source_id": sourceID, "job_id": job.ID, "attempt": job.Attempts, "mode": mode}, now); err != nil {
		return Generation{}, err
	}
	return generation, tx.Commit()
}

func (s *Inventory) require(ctx context.Context, tx *sql.Tx, lease jobs.Lease, id string) (Generation, library.Source, media.SourceRoot, error) {
	if _, err := s.queue.RequireLeaseTx(ctx, tx, lease); err != nil {
		return Generation{}, library.Source{}, media.SourceRoot{}, err
	}
	generation, err := scanGeneration(tx.QueryRowContext(ctx, generationSelect+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Generation{}, library.Source{}, media.SourceRoot{}, ErrGeneration
	}
	if err != nil {
		return Generation{}, library.Source{}, media.SourceRoot{}, err
	}
	if generation.JobID != lease.Job.ID || generation.Attempt != lease.Job.Attempts || generation.owner != lease.Owner || (generation.State != "walking" && generation.State != "ready") {
		return Generation{}, library.Source{}, media.SourceRoot{}, ErrGeneration
	}
	source, root, err := s.sources.RequireConfigTx(ctx, tx, generation.SourceID, generation.ConfigVersion)
	return generation, source, root, err
}

// RequireReadyTx is the mandatory publisher boundary. Holding staged rows or a
// generation ID is not proof of completion, health, current config, or lease owner.
func (s *Inventory) RequireReadyTx(ctx context.Context, tx *sql.Tx, lease jobs.Lease, id string) (Generation, library.Source, media.SourceRoot, error) {
	generation, source, root, err := s.require(ctx, tx, lease, id)
	if err != nil {
		return Generation{}, library.Source{}, media.SourceRoot{}, err
	}
	if generation.State != "ready" || !generation.Healthy || generation.CompletedAt == nil {
		return Generation{}, library.Source{}, media.SourceRoot{}, ErrGeneration
	}
	return generation, source, root, nil
}

func validDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (s *Inventory) Record(ctx context.Context, lease jobs.Lease, id string, files []File) (Generation, error) {
	if len(files) < 1 || len(files) > s.policy.BatchSize {
		return Generation{}, ErrLimit
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Generation{}, err
	}
	defer tx.Rollback()
	generation, _, _, err := s.require(ctx, tx, lease, id)
	if err != nil {
		return Generation{}, err
	}
	if generation.State != "walking" {
		return Generation{}, ErrGeneration
	}
	var added, bytes int64
	for _, file := range files {
		if !validRelative(file.RelativePath) || file.SizeBytes < 0 || file.SizeBytes > s.policy.MaxFileBytes || !validDigest(file.SHA256) || !validDigest(file.QuickFingerprint) || !identityPattern.MatchString(file.Identity) {
			return Generation{}, ErrUnsafeEntry
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO inventory_files(generation_id,relative_path,size_bytes,mtime_ns,file_identity,quick_fingerprint,sha256) VALUES(?,?,?,?,?,?,?) ON CONFLICT(generation_id,relative_path) DO NOTHING`, id, file.RelativePath, file.SizeBytes, file.MtimeNS, file.Identity, file.QuickFingerprint, file.SHA256)
		if err != nil {
			return Generation{}, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return Generation{}, err
		}
		if count == 0 {
			existing, err := readFile(tx.QueryRowContext(ctx, fileSelect+` WHERE generation_id=? AND relative_path=?`, id, file.RelativePath))
			if err != nil {
				return Generation{}, err
			}
			if existing != file {
				return Generation{}, ErrChanged
			}
			continue
		}
		if file.SizeBytes > math.MaxInt64-bytes {
			return Generation{}, ErrLimit
		}
		added++
		bytes += file.SizeBytes
	}
	if added == 0 {
		return generation, nil
	}
	if added > s.policy.MaxFiles-generation.FileCount || bytes > math.MaxInt64-generation.SizeBytes {
		return Generation{}, ErrLimit
	}
	generation.FileCount += added
	generation.SizeBytes += bytes
	if err := s.save(ctx, tx, &generation, s.clock()); err != nil {
		return Generation{}, err
	}
	counts, err := stagedCounts(generation)
	if err != nil {
		return Generation{}, err
	}
	if _, err := s.queue.ProgressTx(ctx, tx, lease, counts, false); err != nil {
		return Generation{}, err
	}
	return generation, tx.Commit()
}

func stagedCounts(generation Generation) (jobs.Counts, error) {
	counts := generation.base
	if generation.FileCount > math.MaxInt64-counts.Total || generation.FileCount > math.MaxInt64-counts.Done || generation.FileCount > math.MaxInt64-counts.Succeeded {
		return jobs.Counts{}, ErrLimit
	}
	counts.Total += generation.FileCount
	counts.Done += generation.FileCount
	counts.Succeeded += generation.FileCount
	return counts, nil
}

const fileSelect = `SELECT relative_path,size_bytes,mtime_ns,file_identity,quick_fingerprint,sha256 FROM inventory_files`

func readFile(row interface{ Scan(...any) error }) (File, error) {
	var file File
	err := row.Scan(&file.RelativePath, &file.SizeBytes, &file.MtimeNS, &file.Identity, &file.QuickFingerprint, &file.SHA256)
	return file, err
}

func (s *Inventory) save(ctx context.Context, tx *sql.Tx, generation *Generation, now time.Time) error {
	previous := generation.Version
	generation.Version++
	if now.After(generation.UpdatedAt) {
		generation.UpdatedAt = now
	}
	result, err := tx.ExecContext(ctx, `UPDATE inventory_generations SET state=?,healthy=?,file_count=?,metadata_count=?,metadata_complete=?,inspect_policy_sha256=?,size_bytes=?,error_code=?,version=?,updated_at=?,completed_at=?,published_at=? WHERE id=? AND version=?`, generation.State, generation.Healthy, generation.FileCount, generation.MetadataCount, generation.MetadataComplete, generation.inspectionPolicy, generation.SizeBytes, generation.ErrorCode, generation.Version, generation.UpdatedAt.UnixMilli(), inventoryTime(generation.CompletedAt), inventoryTime(generation.PublishedAt), generation.ID, previous)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrGeneration
	}
	return store.Change(ctx, tx, "system", "inventory", generation.ID, "upsert", generation.Version, generation, now)
}
func inventoryTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UnixMilli()
}
