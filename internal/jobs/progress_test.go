package jobs

import "testing"

func TestProgressTxCommitsOrRollsBackWithStagedBusinessData(t *testing.T) {
	q, _ := fixture(t)
	job := enqueued(t, q, "staged-progress", 0)
	lease := claimed(t, q)
	if _, err := q.db.Exec(`CREATE TABLE staged_work(id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"caller rollback", "event failure", "commit"} {
		if mode == "event failure" {
			if _, err := q.db.Exec(`CREATE TRIGGER fail_progress_event BEFORE INSERT ON outbox_events BEGIN SELECT RAISE(ABORT,'injected progress failure'); END`); err != nil {
				t.Fatal(err)
			}
		}
		tx, err := q.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO staged_work(id) VALUES('one')`); err != nil {
			t.Fatal(err)
		}
		_, err = q.ProgressTx(t.Context(), tx, lease, Counts{Total: 1, Done: 1, Succeeded: 1}, false)
		if mode == "event failure" {
			if err == nil {
				t.Fatal("progress committed without outbox")
			}
		} else if err != nil {
			t.Fatal(err)
		}
		if mode == "commit" {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		} else {
			tx.Rollback()
		}
		if mode == "event failure" {
			if _, err := q.db.Exec(`DROP TRIGGER fail_progress_event`); err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if err := q.db.QueryRow(`SELECT COUNT(*) FROM staged_work`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		current, err := q.Get(t.Context(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if mode == "commit" {
			if count != 1 || current.Done != 1 || current.Succeeded != 1 {
				t.Fatal("staged work/progress diverged", count, current)
			}
		} else if count != 0 || current.Done != 0 || current.Version != lease.Job.Version {
			t.Fatal("partial staged work/progress", count, current)
		}
	}
}
