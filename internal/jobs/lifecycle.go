package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"gallerygo/internal/store"
)

func (q *Queue) Complete(ctx context.Context, lease Lease) error {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := q.CompleteTx(ctx, tx, lease); err != nil {
		return err
	}
	return tx.Commit()
}

// CompleteTx lets a scanner/publisher commit its business rows, job completion,
// event/outbox, and attempt result atomically under the same live lease fence.
func (q *Queue) CompleteTx(ctx context.Context, tx *sql.Tx, lease Lease) (Job, error) {
	job, err := q.RequireLeaseTx(ctx, tx, lease)
	if err != nil {
		return Job{}, err
	}
	if !job.TotalKnown || job.Done != job.Total {
		return Job{}, ErrProgress
	}
	base, err := q.attemptBase(ctx, tx, job)
	if err != nil {
		return Job{}, err
	}
	delta := job.Counts.delta(base)
	job.Status = "succeeded"
	if delta.Failed > 0 || delta.Cancelled > 0 {
		job.Status = "failed"
		setFailure(&job, "PROCESSING_FAILED")
	}
	now := q.clock()
	job.FinishedAt = &now
	clearLease(&job)
	if err := q.endAttempt(ctx, tx, job, job.Status, now); err != nil {
		return Job{}, err
	}
	if err := q.save(ctx, tx, &job, now); err != nil {
		return Job{}, err
	}
	return job, nil
}

func (q *Queue) Fail(ctx context.Context, lease Lease, code string, retryable bool) error {
	if failureMessages[code] == "" {
		return ErrInvalidJob
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := q.leaseInTx(ctx, tx, lease, true)
	if err != nil {
		return err
	}
	now := q.clock()
	finishInterrupted(&job)
	attemptStatus := "failed"
	if job.CancelRequested {
		job.Status, attemptStatus = "cancelled", "cancelled"
	} else {
		setFailure(&job, code)
		job.Status = "failed"
		if retryable {
			job.Status = "dead"
			if job.Attempts < job.MaxAttempts {
				job.Status = "queued"
				job.NextRunAt = now.Add(q.backoff(job.Attempts))
			}
		}
	}
	if job.Status != "queued" {
		job.FinishedAt = &now
	}
	clearLease(&job)
	if err := q.endAttempt(ctx, tx, job, attemptStatus, now); err != nil {
		return err
	}
	if err := q.save(ctx, tx, &job, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (q *Queue) AcknowledgeCancel(ctx context.Context, lease Lease) error {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := q.leaseInTx(ctx, tx, lease, true)
	if err != nil {
		return err
	}
	if !job.CancelRequested {
		return ErrState
	}
	now := q.clock()
	finishInterrupted(&job)
	job.Status, job.FinishedAt = "cancelled", &now
	clearLease(&job)
	if err := q.endAttempt(ctx, tx, job, "cancelled", now); err != nil {
		return err
	}
	if err := q.save(ctx, tx, &job, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (q *Queue) recoverTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, jobSelect+` WHERE status='running' AND lease_expires_at<=? ORDER BY lease_expires_at,id LIMIT 100`, now.UnixMilli())
	if err != nil {
		return err
	}
	items := make([]Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return err
		}
		items = append(items, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, job := range items {
		finishInterrupted(&job)
		attemptStatus := "failed"
		if job.CancelRequested {
			job.Status, attemptStatus = "cancelled", "cancelled"
		} else {
			setFailure(&job, "LEASE_EXPIRED")
			job.Status = "dead"
			if job.Attempts < job.MaxAttempts {
				job.Status = "queued"
				job.NextRunAt = now.Add(q.backoff(job.Attempts))
			}
		}
		if job.Status != "queued" {
			job.FinishedAt = &now
		}
		clearLease(&job)
		if err := q.endAttempt(ctx, tx, job, attemptStatus, now); err != nil {
			return err
		}
		if err := q.save(ctx, tx, &job, now); err != nil {
			return err
		}
		if err := store.Audit(ctx, tx, "", "job_lease_recovered", "job", job.ID, map[string]any{"attempt": job.Attempts, "status": job.Status}, now); err != nil {
			return err
		}
	}
	return nil
}

func finishInterrupted(job *Job) {
	// Unfinished units in a closed attempt are cancelled work, not success.
	// Closing them preserves monotonic cumulative counts when a retry adds units.
	job.Cancelled += job.Total - job.Done
	job.Done = job.Total
}

func clearLease(job *Job) { job.leaseOwner = ""; job.leaseUntil = 0 }
func setFailure(job *Job, code string) {
	message := failureMessages[code]
	job.ErrorCode = &code
	job.LastError = &message
}

func (q *Queue) attemptBase(ctx context.Context, tx *sql.Tx, job Job) (Counts, error) {
	var data string
	if err := tx.QueryRowContext(ctx, `SELECT base_counts_json FROM job_attempts WHERE job_id=? AND attempt=?`, job.ID, job.Attempts).Scan(&data); err != nil {
		return Counts{}, err
	}
	var base Counts
	err := json.Unmarshal([]byte(data), &base)
	return base, err
}

func (q *Queue) endAttempt(ctx context.Context, tx *sql.Tx, job Job, status string, now time.Time) error {
	if err := q.attemptCounts(ctx, tx, job); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE job_attempts SET status=?,finished_at=?,error_code=?,message=? WHERE job_id=? AND attempt=? AND status='running'`, status, now.UnixMilli(), job.ErrorCode, job.LastError, job.ID, job.Attempts)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (q *Queue) backoff(attempt int) time.Duration {
	delay := q.policy.RetryBase
	for i := 1; i < attempt; i++ {
		if delay >= q.policy.RetryMax/2 {
			return q.policy.RetryMax
		}
		delay *= 2
	}
	if delay > q.policy.RetryMax {
		return q.policy.RetryMax
	}
	return delay
}
