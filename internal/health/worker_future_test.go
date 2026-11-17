package health

import (
	"errors"
	"testing"
	"time"
)

func TestReadinessRejectsFutureWorkerHeartbeat(t *testing.T) {
	service, queue, _ := healthFixture(t)
	if err := queue.RegisterWorker(t.Context(), "future-health-worker", "worker", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.ExecContext(t.Context(), `UPDATE workers SET last_seen_at=? WHERE id='future-health-worker'`, time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Check(t.Context()); !errors.Is(err, ErrWorkerUnavailable) {
		t.Fatal("future worker heartbeat became readiness evidence", err)
	}
	if err := queue.RegisterWorker(t.Context(), "current-health-worker", "worker", 1); err != nil {
		t.Fatal(err)
	}
	state, err := service.Check(t.Context())
	if err != nil || state.Worker != "ok" || state.Status != "ok" {
		t.Fatal("fresh worker should satisfy readiness", state, err)
	}
	if err := queue.StopWorker(t.Context(), "current-health-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Check(t.Context()); !errors.Is(err, ErrWorkerUnavailable) {
		t.Fatal("stopped worker hid future-only evidence", err)
	}
}
