package jobs

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestConfigurationCancellationIsBatchedScopedAndAtomic(t *testing.T) {
	q, _ := fixture(t)
	tx, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for index := range 110 {
		_, created, err := q.EnqueueTx(t.Context(), tx, Input{Kind: "scan", ResourceKey: "source:changed", Dedupe: fmt.Sprintf("scan:changed:%03d", index), Payload: json.RawMessage(`{}`)})
		if err != nil || !created {
			t.Fatal(created, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	other := enqueued(t, q, "other", -1)
	lease := claimed(t, q)
	if _, err := q.Progress(t.Context(), lease, Counts{Total: 4, Done: 1, Succeeded: 1}, true); err != nil {
		t.Fatal(err)
	}
	var lastID string
	if err := q.db.QueryRow(`SELECT MAX(id) FROM jobs WHERE resource_key='source:changed'`).Scan(&lastID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_last_cancellation BEFORE INSERT ON outbox_events WHEN (SELECT resource_id FROM change_log WHERE seq=NEW.change_seq)='%s' BEGIN SELECT RAISE(ABORT,'injected second-batch failure'); END`, lastID)); err != nil {
		t.Fatal(err)
	}
	tx, err = q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CancelResourceTx(t.Context(), tx, "source:changed", "configuration-admin"); err == nil {
		t.Fatal("later batch committed without outbox")
	}
	tx.Rollback()
	var cancelled int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE cancel_requested=1`).Scan(&cancelled); err != nil || cancelled != 0 {
		t.Fatal("earlier cancellation batch survived rollback", cancelled, err)
	}
	if _, err := q.db.Exec(`DROP TRIGGER fail_last_cancellation`); err != nil {
		t.Fatal(err)
	}
	tx, err = q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	count, err := q.CancelResourceTx(t.Context(), tx, "source:changed", "configuration-admin")
	if err != nil || count != 110 {
		t.Fatal(count, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err := q.Get(t.Context(), lease.Job.ID)
	if err != nil || current.Status != "running" || !current.CancelRequested || current.Done != 1 || current.Succeeded != 1 {
		t.Fatal("running cancellation falsified progress", current, err)
	}
	requireError(t, q.Heartbeat(t.Context(), lease), ErrCancelled)
	untouched, err := q.Get(t.Context(), other.ID)
	if err != nil || untouched.Status != "queued" || untouched.CancelRequested {
		t.Fatal("another resource was cancelled", untouched, err)
	}
	tx, err = q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	count, err = q.CancelResourceTx(t.Context(), tx, "source:changed", "configuration-admin")
	if err != nil || count != 0 {
		t.Fatal("already requested jobs were changed again", count, err)
	}
	_, err = q.CancelResourceTx(t.Context(), tx, "/private/path", "")
	requireError(t, err, ErrInvalidJob)
}
