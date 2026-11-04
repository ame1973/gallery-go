package jobs

import (
	"context"
	"database/sql"
	"time"

	"gallerygo/internal/store"
)

func requestCancel(job *Job, now time.Time) error {
	if job.Status != "queued" && job.Status != "running" {
		return ErrState
	}
	job.CancelRequested = true
	if job.Status == "queued" {
		job.Status, job.FinishedAt = "cancelled", &now
		finishInterrupted(job)
		clearLease(job)
	}
	return nil
}

// CancelResourceTx invalidates queued/running work as part of an authorized
// configuration change. The caller must authorize before invoking it and commit
// its configuration/events with this transaction. Batches bound Go memory, not the
// atomicity boundary; failure in a later batch rolls back every earlier change.
func (q *Queue) CancelResourceTx(ctx context.Context, tx *sql.Tx, resourceKey, actorID string) (int, error) {
	if !keyPattern.MatchString(resourceKey) {
		return 0, ErrInvalidJob
	}
	now, after, total := q.clock(), "", 0
	for {
		rows, err := tx.QueryContext(ctx, jobSelect+` WHERE (resource_key=? OR id IN (SELECT job_id FROM job_resources WHERE resource_key=?)) AND id>? AND status IN ('queued','running') AND cancel_requested=0 ORDER BY id LIMIT 100`, resourceKey, resourceKey, after)
		if err != nil {
			return 0, err
		}
		batch := make([]Job, 0, 100)
		for rows.Next() {
			job, err := scanJob(rows)
			if err != nil {
				rows.Close()
				return 0, err
			}
			batch = append(batch, job)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, err
		}
		if len(batch) == 0 {
			return total, nil
		}
		for _, job := range batch {
			if err := requestCancel(&job, now); err != nil {
				return 0, err
			}
			if err := q.save(ctx, tx, &job, now); err != nil {
				return 0, err
			}
			if err := store.Audit(ctx, tx, actorID, "job_cancelled_by_configuration", "job", job.ID, map[string]any{"resource_key": resourceKey, "attempt": job.Attempts, "status": job.Status}, now); err != nil {
				return 0, err
			}
			after = job.ID
			total++
		}
	}
}
