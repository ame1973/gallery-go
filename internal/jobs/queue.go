package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"gallerygo/internal/store"
)

type Queue struct {
	db     *sql.DB
	policy Policy
	now    func() time.Time
}

func New(db *sql.DB, policy Policy) (*Queue, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &Queue{db: db, policy: policy, now: time.Now}, nil
}

func (q *Queue) clock() time.Time { return q.now().UTC().Truncate(time.Millisecond) }

type Input struct {
	Kind        string
	ResourceKey string
	Dedupe      string
	Payload     json.RawMessage
	Priority    int
	ActorID     string
}

// EnqueueTx must run in the caller's authorized business transaction. An active
// duplicate is returned for explicit coalescing/conflict handling by that caller;
// a different scan/reprocess request is never silently swallowed here.
func (q *Queue) EnqueueTx(ctx context.Context, tx *sql.Tx, input Input) (Job, bool, error) {
	var object map[string]json.RawMessage
	if !kindPattern.MatchString(input.Kind) || !keyPattern.MatchString(input.ResourceKey) || !keyPattern.MatchString(input.Dedupe) || input.Priority < -100 || input.Priority > 100 || len(input.Payload) > 16<<10 || !utf8.Valid(input.Payload) || json.Unmarshal(input.Payload, &object) != nil || object == nil {
		return Job{}, false, ErrInvalidJob
	}
	existing, err := scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE dedupe_key=?`, input.Dedupe))
	if err == nil {
		if existing.Status != "queued" && existing.Status != "running" {
			return Job{}, false, ErrResourceBusy
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, err
	}
	now := q.clock()
	job := Job{ID: store.NewID(), Kind: input.Kind, ResourceKey: input.ResourceKey, Status: "queued", Priority: input.Priority, MaxAttempts: q.policy.MaxAttempts, NextRunAt: now, Version: 1, CreatedAt: now, UpdatedAt: now, Payload: input.Payload, Dedupe: input.Dedupe, creationTx: tx}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,kind,resource_key,dedupe_key,dedupe_token,payload_json,status,priority,max_attempts,next_run_at,created_at,updated_at,version,created_by_user_id) VALUES(?,?,?,?,?,?,'queued',?,?,?,?,?,1,?)`, job.ID, job.Kind, job.ResourceKey, job.Dedupe, job.Dedupe, string(job.Payload), job.Priority, job.MaxAttempts, now.UnixMilli(), now.UnixMilli(), now.UnixMilli(), nullableString(input.ActorID)); err != nil {
		return Job{}, false, err
	}
	if err := q.BindResourceTx(ctx, tx, job, job.ResourceKey); err != nil {
		return Job{}, false, err
	}
	if err := store.Change(ctx, tx, "system", "job", job.ID, "upsert", job.Version, job, now); err != nil {
		return Job{}, false, err
	}
	if err := store.Audit(ctx, tx, input.ActorID, "job_queued", "job", job.ID, map[string]string{"kind": job.Kind, "resource_key": job.ResourceKey}, now); err != nil {
		return Job{}, false, err
	}
	return job, true, nil
}

func (q *Queue) Get(ctx context.Context, id string) (Job, error) {
	return scanJob(q.db.QueryRowContext(ctx, jobSelect+` WHERE id=?`, id))
}

func (q *Queue) Claim(ctx context.Context, workerID string, kinds []string) (Lease, error) {
	if !keyPattern.MatchString(workerID) || len(kinds) == 0 || len(kinds) > 32 {
		return Lease{}, ErrInvalidJob
	}
	for _, kind := range kinds {
		if !kindPattern.MatchString(kind) {
			return Lease{}, ErrInvalidJob
		}
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback()
	now := q.clock()
	if err := q.recoverTx(ctx, tx, now); err != nil {
		return Lease{}, err
	}
	data, _ := json.Marshal(kinds)
	job, err := scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE status='queued' AND cancel_requested=0 AND next_run_at<=? AND attempts<max_attempts AND kind IN (SELECT value FROM json_each(?)) ORDER BY priority DESC,created_at,id LIMIT 1`, now.UnixMilli(), string(data)))
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return Lease{}, err
		}
		return Lease{}, ErrNoWork
	}
	if err != nil {
		return Lease{}, err
	}
	job.Status, job.Attempts, job.leaseOwner, job.leaseUntil = "running", job.Attempts+1, store.NewID(), now.Add(q.policy.Lease).UnixMilli()
	job.ErrorCode, job.LastError, job.FinishedAt = nil, nil, nil
	job.TotalKnown = false
	if job.StartedAt == nil {
		job.StartedAt = &now
	}
	base, _ := json.Marshal(job.Counts)
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_attempts(job_id,attempt,started_at,status,worker_id,base_counts_json,counts_json) VALUES(?,?,?,'running',?,?,'{}')`, job.ID, job.Attempts, now.UnixMilli(), workerID, string(base)); err != nil {
		return Lease{}, err
	}
	if err := q.save(ctx, tx, &job, now); err != nil {
		return Lease{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET worker_id=?,heartbeat_at=? WHERE id=?`, workerID, now.UnixMilli(), job.ID); err != nil {
		return Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, err
	}
	return Lease{Job: job, Owner: job.leaseOwner}, nil
}

// RequireLeaseTx fences a publisher in the same transaction as its business
// write. An expired owner, prior attempt, cancelled job, or reclaimed lease cannot
// publish merely because it still holds an old in-memory Lease value.
func (q *Queue) RequireLeaseTx(ctx context.Context, tx *sql.Tx, lease Lease) (Job, error) {
	return q.leaseInTx(ctx, tx, lease, false)
}

func (q *Queue) leaseInTx(ctx context.Context, tx *sql.Tx, lease Lease, allowCancel bool) (Job, error) {
	job, err := scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE id=?`, lease.Job.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrLeaseLost
	}
	if err != nil {
		return Job{}, err
	}
	if job.Status != "running" || lease.Owner == "" || job.leaseOwner != lease.Owner || job.Attempts != lease.Job.Attempts || job.leaseUntil <= q.clock().UnixMilli() {
		return Job{}, ErrLeaseLost
	}
	if job.CancelRequested && !allowCancel {
		return Job{}, ErrCancelled
	}
	return job, nil
}

func (q *Queue) Heartbeat(ctx context.Context, lease Lease) error {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := q.HeartbeatTx(ctx, tx, lease); err != nil {
		return err
	}
	return tx.Commit()
}

// HeartbeatTx refreshes a still-live owned lease during a long DB-only publish.
// It never revives expiry or waits for another connection while holding a writer.
func (q *Queue) HeartbeatTx(ctx context.Context, tx *sql.Tx, lease Lease) error {
	job, err := q.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return err
	}
	now := q.clock()
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET heartbeat_at=max(COALESCE(heartbeat_at,0),?),lease_expires_at=max(lease_expires_at,?) WHERE id=?`, now.UnixMilli(), now.Add(q.policy.Lease).UnixMilli(), job.ID)
	return err
}

func (q *Queue) Progress(ctx context.Context, lease Lease, counts Counts, totalKnown bool) (Job, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	job, err := q.ProgressTx(ctx, tx, lease, counts, totalKnown)
	if err != nil {
		return Job{}, err
	}
	return job, tx.Commit()
}

// ProgressTx commits staged business work and its truthful progress together.
// The caller owns the transaction; filesystem work must finish before entering it.
func (q *Queue) ProgressTx(ctx context.Context, tx *sql.Tx, lease Lease, counts Counts, totalKnown bool) (Job, error) {
	job, err := q.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return Job{}, err
	}
	if !counts.follows(job.Counts) {
		return Job{}, ErrProgress
	}
	if counts == job.Counts && totalKnown == job.TotalKnown {
		return job, nil
	}
	job.Counts, job.TotalKnown = counts, totalKnown
	now := q.clock()
	if err := q.attemptCounts(ctx, tx, job); err != nil {
		return Job{}, err
	}
	if err := q.save(ctx, tx, &job, now); err != nil {
		return Job{}, err
	}
	return job, nil
}

func (q *Queue) attemptCounts(ctx context.Context, tx *sql.Tx, job Job) error {
	var data string
	if err := tx.QueryRowContext(ctx, `SELECT base_counts_json FROM job_attempts WHERE job_id=? AND attempt=?`, job.ID, job.Attempts).Scan(&data); err != nil {
		return err
	}
	var base Counts
	if err := json.Unmarshal([]byte(data), &base); err != nil {
		return err
	}
	delta := job.Counts.delta(base)
	if !delta.valid() {
		return ErrProgress
	}
	encoded, _ := json.Marshal(delta)
	_, err := tx.ExecContext(ctx, `UPDATE job_attempts SET counts_json=? WHERE job_id=? AND attempt=?`, string(encoded), job.ID, job.Attempts)
	return err
}

func (q *Queue) save(ctx context.Context, tx *sql.Tx, job *Job, now time.Time) error {
	previous := job.Version
	job.Version++
	if now.After(job.UpdatedAt) {
		job.UpdatedAt = now
	}
	var activeDedupe any
	if job.Status == "queued" || job.Status == "running" {
		activeDedupe = job.Dedupe
	}
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET status=?,cancel_requested=?,attempts=?,max_attempts=?,next_run_at=?,total=?,done=?,succeeded=?,failed=?,skipped=?,cancelled=?,total_known=?,error_code=?,last_error=?,version=?,updated_at=?,started_at=?,finished_at=?,dedupe_key=?,lease_owner=?,lease_expires_at=? WHERE id=? AND version=?`, job.Status, job.CancelRequested, job.Attempts, job.MaxAttempts, job.NextRunAt.UnixMilli(), job.Total, job.Done, job.Succeeded, job.Failed, job.Skipped, job.Cancelled, job.TotalKnown, job.ErrorCode, job.LastError, job.Version, job.UpdatedAt.UnixMilli(), optionalMillis(job.StartedAt), optionalMillis(job.FinishedAt), activeDedupe, nullableString(job.leaseOwner), nullableMillis(job.leaseUntil), job.ID, previous)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrState
	}
	if job.Status != "running" {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET worker_id=NULL,heartbeat_at=NULL WHERE id=?`, job.ID); err != nil {
			return err
		}
	}
	return store.Change(ctx, tx, "system", "job", job.ID, "upsert", job.Version, job, now)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func nullableMillis(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
func optionalMillis(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UnixMilli()
}
