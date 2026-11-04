package jobs

import (
	"context"
	"database/sql"
)

// BindResourceTx adds a private dependency only to a newly enqueued job in its
// original creation transaction. Callers can stream a large target set in bounded
// batches; an active dedupe result cannot be silently expanded into a new intent.
func (q *Queue) BindResourceTx(ctx context.Context, tx *sql.Tx, job Job, resourceKey string) error {
	if tx == nil || !keyPattern.MatchString(job.ID) || !keyPattern.MatchString(resourceKey) {
		return ErrInvalidJob
	}
	if job.creationTx != tx {
		return ErrState
	}
	var fresh bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE id=? AND status='queued' AND attempts=0 AND cancel_requested=0 AND version=1)`, job.ID).Scan(&fresh); err != nil {
		return err
	}
	if !fresh {
		return ErrState
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO job_resources(job_id,resource_key) VALUES(?,?) ON CONFLICT(job_id,resource_key) DO NOTHING`, job.ID, resourceKey)
	return err
}
