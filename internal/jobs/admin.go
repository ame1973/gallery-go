package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

type Admin struct {
	queue *Queue
	auth  *auth.Service
}

func NewAdmin(queue *Queue, authentication *auth.Service) *Admin {
	return &Admin{queue: queue, auth: authentication}
}

func (a *Admin) Get(ctx context.Context, current auth.Session, id string) (Job, error) {
	tx, err := a.queue.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	if _, err := a.auth.RequireAdmin(ctx, tx, current); err != nil {
		return Job{}, err
	}
	return scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE id=?`, id))
}

func (a *Admin) List(ctx context.Context, current auth.Session, after, status string, limit int) ([]Job, error) {
	if limit < 1 || limit > 101 || !validStatusFilter(status) {
		return nil, ErrInvalidJob
	}
	tx, err := a.queue.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := a.auth.RequireAdmin(ctx, tx, current); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, jobSelect+` WHERE id>? AND (?='' OR status=?) ORDER BY id LIMIT ?`, after, status, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, job)
	}
	return items, rows.Err()
}

func validStatusFilter(value string) bool {
	switch value {
	case "", "queued", "running", "succeeded", "failed", "cancelled", "dead":
		return true
	default:
		return false
	}
}

func (a *Admin) Attempts(ctx context.Context, current auth.Session, id string, after, limit int) ([]Attempt, error) {
	if limit < 1 || limit > 101 || after < 0 {
		return nil, ErrInvalidJob
	}
	tx, err := a.queue.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := a.auth.RequireAdmin(ctx, tx, current); err != nil {
		return nil, err
	}
	var exists string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE id=?`, id).Scan(&exists); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT attempt,status,started_at,finished_at,error_code,message,counts_json FROM job_attempts WHERE job_id=? AND attempt>? ORDER BY attempt LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Attempt, 0)
	for rows.Next() {
		var item Attempt
		var start int64
		var end sql.NullInt64
		var counts string
		if err := rows.Scan(&item.Number, &item.Status, &start, &end, &item.ErrorCode, &item.Message, &counts); err != nil {
			return nil, err
		}
		item.StartedAt = time.UnixMilli(start).UTC()
		item.FinishedAt = optionalTime(end)
		if err := json.Unmarshal([]byte(counts), &item.Counts); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (a *Admin) Cancel(ctx context.Context, current auth.Session, id string, version int64, key string) (Job, error) {
	return a.mutate(ctx, current, id, version, key, false)
}
func (a *Admin) Retry(ctx context.Context, current auth.Session, id string, version int64, key string) (Job, error) {
	return a.mutate(ctx, current, id, version, key, true)
}

func (a *Admin) mutate(ctx context.Context, current auth.Session, id string, version int64, key string, retry bool) (Job, error) {
	tx, err := a.queue.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	actor, err := a.auth.RequireAdmin(ctx, tx, current)
	if err != nil {
		return Job{}, err
	}
	op := "job.cancel"
	if retry {
		op = "job.retry"
	}
	metadata, _ := json.Marshal(struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}{id, version})
	command := store.Command{Actor: actor.User.ID, Key: key, Operation: op, Metadata: string(metadata)}
	now := a.queue.clock()
	var job Job
	if found, err := command.Replay(ctx, tx, now, &job); found || err != nil {
		return job, err
	}
	job, err = scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE id=?`, id))
	if err != nil {
		return Job{}, err
	}
	if job.Version != version {
		return Job{}, auth.ErrConflict
	}
	if retry {
		if job.Status != "failed" && job.Status != "dead" && job.Status != "cancelled" {
			return Job{}, ErrState
		}
		var busy bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE dedupe_key=? AND id!=?)`, job.Dedupe, id).Scan(&busy); err != nil {
			return Job{}, err
		}
		if busy {
			return Job{}, ErrResourceBusy
		}
		// ponytail: preserve all attempt history; after 100 attempts start a new
		// explicit job rather than growing one job's retry budget indefinitely.
		if job.Attempts >= 100 {
			return Job{}, ErrState
		}
		job.MaxAttempts = min(100, job.Attempts+a.queue.policy.MaxAttempts)
		job.Status, job.CancelRequested, job.NextRunAt, job.FinishedAt = "queued", false, now, nil
		clearLease(&job)
	} else {
		if job.Status != "queued" && job.Status != "running" {
			return Job{}, ErrState
		}
		job.CancelRequested = true
		if job.Status == "queued" {
			job.Status = "cancelled"
			job.FinishedAt = &now
			finishInterrupted(&job)
			clearLease(&job)
		}
	}
	if err := a.queue.save(ctx, tx, &job, now); err != nil {
		return Job{}, err
	}
	if err := store.Audit(ctx, tx, actor.User.ID, op, "job", id, map[string]any{"status": job.Status, "attempt": job.Attempts}, now); err != nil {
		return Job{}, err
	}
	if err := command.Save(ctx, tx, job, now); err != nil {
		return Job{}, err
	}
	return job, tx.Commit()
}
