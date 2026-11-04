package jobs

import (
	"testing"
	"time"
)

func TestHeartbeatTxRefreshesAtomicallyWithoutRevivingExpiry(t *testing.T) {
	q, clock := fixture(t)
	job := enqueued(t, q, "long-publish", 0)
	lease := claimed(t, q)
	initial, err := q.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(10 * time.Second)
	tx, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.HeartbeatTx(t.Context(), tx, lease); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	rolledBack, err := q.Get(t.Context(), job.ID)
	if err != nil || rolledBack.leaseUntil != initial.leaseUntil || rolledBack.Version != initial.Version {
		t.Fatal("heartbeat survived caller rollback", rolledBack, err)
	}
	tx, err = q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.HeartbeatTx(t.Context(), tx, lease); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err := q.Get(t.Context(), job.ID)
	if err != nil || current.leaseUntil <= initial.leaseUntil || current.Version != initial.Version {
		t.Fatal("private heartbeat changed business version or failed to renew", current, err)
	}
	*clock = time.UnixMilli(current.leaseUntil)
	tx, err = q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	requireError(t, q.HeartbeatTx(t.Context(), tx, lease), ErrLeaseLost)
}
