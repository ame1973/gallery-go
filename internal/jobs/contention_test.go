package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRunnerRetriesWriterContentionDuringTransactionBoundHeartbeats(t *testing.T) {
	q := runtimeFixture(t)
	q.policy.Concurrency = 1
	job := enqueued(t, q, "long-publication", 0)
	if _, err := q.db.Exec(`CREATE TABLE publication_fixture(id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	committed := make(chan error, 1)
	busyResult := make(chan error, 1)
	handler := func(ctx context.Context, lease Lease) (result error) {
		defer func() { committed <- result }()
		tx, err := q.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := q.HeartbeatTx(ctx, tx, lease); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO publication_fixture VALUES('atomic')`); err != nil {
			return err
		}
		// This real second writer must reach the production 5s busy timeout. The
		// publisher's own connection continues renewing its live 3s lease meanwhile.
		go func() {
			_, err := q.db.ExecContext(ctx, `UPDATE jobs SET version=version WHERE id=?`, lease.Job.ID)
			busyResult <- err
		}()
		deadline := time.NewTimer(6 * time.Second)
		defer deadline.Stop()
		heartbeat := time.NewTicker(250 * time.Millisecond)
		defer heartbeat.Stop()
		for {
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-heartbeat.C:
				if err := q.HeartbeatTx(ctx, tx, lease); err != nil {
					return err
				}
			case <-deadline.C:
				if _, err := q.ProgressTx(ctx, tx, lease, Counts{Total: 1, Done: 1, Succeeded: 1}, true); err != nil {
					return err
				}
				if _, err := q.CompleteTx(ctx, tx, lease); err != nil {
					return err
				}
				return tx.Commit()
			}
		}
	}
	cancel, done := startRunner(t, q, map[string]Handler{"scan": handler})
	defer stopRunner(t, cancel, done)
	select {
	case err := <-committed:
		if err != nil {
			t.Fatal("transient writer contention cancelled a live atomic publisher", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("publication did not finish")
	}
	select {
	case err := <-busyResult:
		if err == nil || !databaseBusy(fmt.Errorf("wrapped: %w", err)) {
			t.Fatal("real SQLite busy error not recognized", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("contending writer did not stop")
	}
	current, err := q.Get(t.Context(), job.ID)
	if err != nil || current.Status != "succeeded" || current.Attempts != 1 || current.Succeeded != 1 {
		t.Fatal("publisher attempt was interrupted or replayed", current, err)
	}
	var count int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM publication_fixture`).Scan(&count); err != nil || count != 1 {
		t.Fatal("business publication missing", count, err)
	}
}

func TestRunnerDoesNotRetryOtherDatabaseOrAuthorityFailuresAsBusy(t *testing.T) {
	q := runtimeFixture(t)
	_, sqlError := q.db.Exec(`NOT VALID SQL`)
	for _, err := range []error{nil, sqlError, sql.ErrNoRows, context.Canceled, ErrLeaseLost, ErrCancelled, errors.New("database is locked")} {
		if databaseBusy(err) {
			t.Fatal("non-busy or untrusted text treated as retryable authority", err)
		}
	}
}
