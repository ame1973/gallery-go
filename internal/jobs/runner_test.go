package jobs

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gallerygo/internal/store"
)

func runtimeFixture(t *testing.T) *Queue {
	t.Helper()
	q, _ := fixture(t)
	q.now = time.Now
	q.policy = Policy{Concurrency: 2, Lease: 3 * time.Second, Heartbeat: 500 * time.Millisecond, Poll: 100 * time.Millisecond, MaxAttempts: 1, RetryBase: 100 * time.Millisecond, RetryMax: time.Second}
	if err := q.policy.Validate(); err != nil {
		t.Fatal(err)
	}
	return q
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for durable worker state")
		case <-ticker.C:
		}
	}
}

func startRunner(t *testing.T, q *Queue, handlers map[string]Handler) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- q.Run(ctx, "worker", handlers) }()
	return cancel, done
}

func stopRunner(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Error("worker shutdown", err)
		}
	case <-time.After(8 * time.Second):
		t.Error("worker did not stop cooperatively")
	}
}

func TestRunnerBoundedConcurrencyLiveHeartbeatsAndCompletion(t *testing.T) {
	q := runtimeFixture(t)
	jobs := []Job{enqueued(t, q, "one", 0), enqueued(t, q, "two", 0), enqueued(t, q, "three", 0)}
	started := make(chan string, 3)
	release := make(chan struct{})
	var active, maximum atomic.Int32
	handler := func(ctx context.Context, lease Lease) error {
		current := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); current > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- lease.Job.ID
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
		counts := lease.Job.Counts
		counts.Total++
		counts.Done++
		counts.Succeeded++
		_, err := q.Progress(ctx, lease, counts, true)
		return err
	}
	cancel, done := startRunner(t, q, map[string]Handler{"scan": handler})
	defer stopRunner(t, cancel, done)
	for range 2 {
		select {
		case <-started:
		case <-time.After(8 * time.Second):
			t.Fatal("worker slots did not start")
		}
	}
	var firstExpiry int64
	if err := q.db.QueryRow(`SELECT MIN(lease_expires_at) FROM jobs WHERE status='running'`).Scan(&firstExpiry); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		var renewed int
		err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status='running' AND lease_expires_at>?`, firstExpiry).Scan(&renewed)
		workers, workerErr := q.Workers(t.Context(), "", 30)
		return err == nil && renewed == 2 && workerErr == nil && len(workers) == 1 && workers[0].LastSeenAt.After(workers[0].StartedAt)
	})
	workers, err := q.Workers(t.Context(), "", 30)
	if err != nil || len(workers) != 1 || workers[0].Slots != 2 || !workers[0].LastSeenAt.After(workers[0].StartedAt) {
		t.Fatal("worker heartbeat was not live", workers, err)
	}
	var running, queued int
	if err := q.db.QueryRow(`SELECT SUM(status='running'),SUM(status='queued') FROM jobs`).Scan(&running, &queued); err != nil || running != 2 || queued != 1 {
		t.Fatal("unbounded claim", running, queued, err)
	}
	close(release)
	eventually(t, func() bool {
		for _, job := range jobs {
			current, err := q.Get(t.Context(), job.ID)
			if err != nil || current.Status != "succeeded" || current.Succeeded != 1 {
				return false
			}
		}
		return true
	})
	if maximum.Load() != 2 {
		t.Fatal("concurrency limit was not exercised", maximum.Load())
	}
}

func TestRunnerCancellationStopsHandlerAndClosesOnlyUnfinishedWork(t *testing.T) {
	q := runtimeFixture(t)
	// Cancellation, not lease timing, is under test. Allow writer contention on
	// loaded test hosts without weakening the separate exact-expiry regressions.
	q.policy.Lease = 10 * time.Second
	admin, owner := adminFixture(t, q)
	job := enqueued(t, q, "cancel-live", 0)
	started := make(chan struct{})
	observed := make(chan error, 1)
	handler := func(ctx context.Context, lease Lease) error {
		if _, err := q.Progress(ctx, lease, Counts{Total: 4, Done: 1, Succeeded: 1}, true); err != nil {
			return err
		}
		close(started)
		<-ctx.Done()
		observed <- context.Cause(ctx)
		return ctx.Err()
	}
	cancel, done := startRunner(t, q, map[string]Handler{"scan": handler})
	defer stopRunner(t, cancel, done)
	select {
	case <-started:
	case <-time.After(8 * time.Second):
		t.Fatal("handler did not start")
	}
	current, err := q.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Cancel(t.Context(), owner, job.ID, current.Version, store.NewID()); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-observed:
		// Recovery can settle a cancelled expired lease before this handler's
		// heartbeat sees the flag. Both causes revoke authority; the durable
		// cancelled status and exact unfinished counts below are mandatory.
		if !errors.Is(cause, ErrCancelled) && !errors.Is(cause, ErrLeaseLost) {
			t.Fatalf("handler did not observe cancellation or lost authority: %v", cause)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("cancel did not interrupt handler")
	}
	eventually(t, func() bool {
		result, err := q.Get(t.Context(), job.ID)
		return err == nil && result.Status == "cancelled" && result.Succeeded == 1 && result.Cancelled == 3 && result.Done == 4
	})
}

func TestRunnerShutdownRequeuesLiveWorkAndStopsRegistry(t *testing.T) {
	q := runtimeFixture(t)
	q.policy.MaxAttempts = 2
	job := enqueued(t, q, "shutdown", 0)
	started := make(chan struct{})
	handler := func(ctx context.Context, lease Lease) error {
		if _, err := q.Progress(ctx, lease, Counts{Total: 2}, true); err != nil {
			return err
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	cancel, done := startRunner(t, q, map[string]Handler{"scan": handler})
	select {
	case <-started:
	case <-time.After(8 * time.Second):
		stopRunner(t, cancel, done)
		t.Fatal("handler did not start")
	}
	stopRunner(t, cancel, done)
	current, err := q.Get(t.Context(), job.ID)
	if err != nil || current.Status != "queued" || current.Attempts != 1 || current.Cancelled != 2 || current.Succeeded != 0 || current.ErrorCode == nil || *current.ErrorCode != "WORKER_STOPPED" {
		t.Fatal("shutdown lost active work", current, err)
	}
	workers, err := q.Workers(t.Context(), "", 30)
	if err != nil || len(workers) != 1 || workers[0].State != "stopped" {
		t.Fatal("worker registry stayed live", workers, err)
	}
	available, err := q.Available(t.Context())
	if err != nil || available {
		t.Fatal("stopped worker advertised availability", err)
	}
}

func TestRuntimeResultSafetyAndAtomicHandlerCompletion(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler func(*Queue) Handler
		status  string
		code    string
	}{
		{"missing progress", func(*Queue) Handler { return func(context.Context, Lease) error { return nil } }, "failed", "INTERNAL_ERROR"},
		{"known failure", func(*Queue) Handler {
			return func(context.Context, Lease) error { return &Failure{Code: "INVALID_MEDIA"} }
		}, "failed", "INVALID_MEDIA"},
		{"untrusted error", func(*Queue) Handler {
			return func(context.Context, Lease) error { return errors.New("/private/path secret subprocess output") }
		}, "dead", "INTERNAL_ERROR"},
		{"unknown code", func(*Queue) Handler {
			return func(context.Context, Lease) error { return &Failure{Code: "/private/path", Retryable: true} }
		}, "dead", "INTERNAL_ERROR"},
		{"atomic publisher", func(q *Queue) Handler {
			return func(ctx context.Context, lease Lease) error {
				if _, err := q.Progress(ctx, lease, Counts{Total: 1, Done: 1, Succeeded: 1}, true); err != nil {
					return err
				}
				tx, err := q.db.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				if _, err := q.RequireLeaseTx(ctx, tx, lease); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO business(id) VALUES('published')`); err != nil {
					return err
				}
				if _, err := q.CompleteTx(ctx, tx, lease); err != nil {
					return err
				}
				return tx.Commit()
			}
		}, "succeeded", ""},
		{"cancel wins completion", func(q *Queue) Handler {
			return func(ctx context.Context, lease Lease) error {
				if _, err := q.Progress(ctx, lease, Counts{Total: 1, Done: 1, Succeeded: 1}, true); err != nil {
					return err
				}
				// Place the flag exactly between handler return and the runtime fence.
				_, err := q.db.ExecContext(ctx, `UPDATE jobs SET cancel_requested=1 WHERE id=?`, lease.Job.ID)
				return err
			}
		}, "cancelled", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := runtimeFixture(t)
			if _, err := q.db.Exec(`CREATE TABLE business(id TEXT PRIMARY KEY)`); err != nil {
				t.Fatal(err)
			}
			job := enqueued(t, q, "result", 0)
			lease := claimed(t, q)
			if err := q.execute(t.Context(), lease, test.handler(q)); err != nil {
				t.Fatal(err)
			}
			result, err := q.Get(t.Context(), job.ID)
			if err != nil || result.Status != test.status {
				t.Fatal(result, err)
			}
			if test.code != "" && (result.ErrorCode == nil || *result.ErrorCode != test.code) {
				t.Fatal("wrong diagnostic", result.ErrorCode)
			}
			if result.LastError != nil && strings.Contains(*result.LastError, "/private/path") {
				t.Fatal("raw error leaked into job history")
			}
			if test.name == "atomic publisher" {
				var count int
				if err := q.db.QueryRow(`SELECT COUNT(*) FROM business`).Scan(&count); err != nil || count != 1 {
					t.Fatal("business publish missing", count, err)
				}
			}
		})
	}
	if strings.Contains((&Failure{Code: "/private/path"}).Error(), "/private/path") {
		t.Fatal("unknown failure exposed private code")
	}
}

func TestRuntimeLeaseLossInterruptsWithoutPublishing(t *testing.T) {
	q := runtimeFixture(t)
	job := enqueued(t, q, "lease-lost", 0)
	lease := claimed(t, q)
	if _, err := q.db.Exec(`UPDATE jobs SET lease_expires_at=0 WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	handler := func(ctx context.Context, _ Lease) error {
		select {
		case <-ctx.Done():
			requireError(t, context.Cause(ctx), ErrLeaseLost)
			return ctx.Err()
		case <-time.After(8 * time.Second):
			return errors.New("stale handler was not cancelled")
		}
	}
	if err := q.execute(t.Context(), lease, handler); err != nil {
		t.Fatal(err)
	}
	result, err := q.Get(t.Context(), job.ID)
	if err != nil || result.Status != "running" || result.Done != 0 {
		t.Fatal("stale owner rewrote job", result, err)
	}
	_, err = q.Claim(t.Context(), "replacement", []string{"scan"})
	requireError(t, err, ErrNoWork)
	result, err = q.Get(t.Context(), job.ID)
	if err != nil || result.Status != "dead" {
		t.Fatal("expired attempt was not recovered", result, err)
	}
}

func TestRunnerHeartbeatFailureStopsClaimsAndPreservesRecoverableWork(t *testing.T) {
	q := runtimeFixture(t)
	q.policy.MaxAttempts = 2
	job := enqueued(t, q, "registry-failure", 0)
	if _, err := q.db.Exec(`CREATE TRIGGER fail_worker_heartbeat BEFORE UPDATE ON workers WHEN NEW.state='running' BEGIN SELECT RAISE(ABORT,'injected worker failure'); END`); err != nil {
		t.Fatal(err)
	}
	handler := func(ctx context.Context, lease Lease) error {
		<-ctx.Done()
		return ctx.Err()
	}
	cancel, done := startRunner(t, q, map[string]Handler{"scan": handler})
	defer cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "injected worker failure") {
			t.Fatal("worker dependency failure was hidden", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("worker kept claiming after heartbeat failure")
	}
	current, err := q.Get(t.Context(), job.ID)
	if err != nil || current.Status != "queued" || current.Attempts != 1 {
		t.Fatal("dependency failure lost the active job", current, err)
	}
	available, err := q.Available(t.Context())
	if err != nil || available {
		t.Fatal("failed worker still advertised readiness", err)
	}
}

func TestRunnerRejectsUnsupportedRegistrations(t *testing.T) {
	q := runtimeFixture(t)
	noop := func(context.Context, Lease) error { return nil }
	for _, test := range []struct {
		role     string
		handlers map[string]Handler
	}{
		{"invalid", map[string]Handler{"scan": noop}},
		{"worker", nil},
		{"serve", map[string]Handler{"invalid-kind": noop}},
		{"worker", map[string]Handler{"scan": nil}},
	} {
		requireError(t, q.Run(t.Context(), test.role, test.handlers), ErrInvalidJob)
	}
}
