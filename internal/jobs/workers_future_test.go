package jobs

import (
	"testing"
	"time"
)

func TestWorkerReadinessRejectsFutureHeartbeat(t *testing.T) {
	q, clock := fixture(t)
	if err := q.RegisterWorker(t.Context(), "future-worker", "worker", 1); err != nil {
		t.Fatal(err)
	}
	available, err := q.Available(t.Context())
	if err != nil || !available {
		t.Fatal("heartbeat at the current instant must be available", available, err)
	}

	// A wall-clock rollback leaves the committed heartbeat in the future.
	*clock = clock.Add(-time.Millisecond)
	available, err = q.Available(t.Context())
	if err != nil || available {
		t.Fatal("future heartbeat must not establish worker availability", available, err)
	}
	if err := q.WorkerHeartbeat(t.Context(), "future-worker"); err != nil {
		t.Fatal(err)
	}
	available, err = q.Available(t.Context())
	if err != nil || available {
		t.Fatal("a monotonic future heartbeat must remain untrusted", available, err)
	}

	if err := q.RegisterWorker(t.Context(), "current-worker", "worker", 1); err != nil {
		t.Fatal(err)
	}
	available, err = q.Available(t.Context())
	if err != nil || !available {
		t.Fatal("another current heartbeat must establish availability", available, err)
	}
	if err := q.StopWorker(t.Context(), "current-worker"); err != nil {
		t.Fatal(err)
	}
	available, err = q.Available(t.Context())
	if err != nil || available {
		t.Fatal("a stopped worker must not hide a future heartbeat", available, err)
	}

	*clock = clock.Add(time.Millisecond)
	available, err = q.Available(t.Context())
	if err != nil || !available {
		t.Fatal("heartbeat should become current when the clock catches up", available, err)
	}
	*clock = clock.Add(q.policy.Lease)
	available, err = q.Available(t.Context())
	if err != nil || available {
		t.Fatal("heartbeat at the lease boundary must be stale", available, err)
	}
}
