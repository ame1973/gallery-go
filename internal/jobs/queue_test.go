package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

func fixture(t *testing.T) (*Queue, *time.Time) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	q, err := New(db, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return clock }
	return q, &clock
}

func enqueued(t *testing.T, q *Queue, resource string, priority int) Job {
	t.Helper()
	tx, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	job, created, err := q.EnqueueTx(t.Context(), tx, Input{Kind: "scan", ResourceKey: "source:" + resource, Dedupe: "scan:" + resource, Payload: json.RawMessage(`{"source_id":"opaque","private":"not-for-export"}`), Priority: priority})
	if err != nil || !created {
		t.Fatal("enqueue", created, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return job
}

func claimed(t *testing.T, q *Queue) Lease {
	t.Helper()
	lease, err := q.Claim(t.Context(), "worker-one", []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func requireError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v want=%v", err, want)
	}
}

func adminFixture(t *testing.T, q *Queue) (*Admin, auth.Session) {
	t.Helper()
	a, err := auth.New(q.db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	const password = "a private job administration test phrase"
	if _, err := a.Bootstrap(t.Context(), "owner", password); err != nil {
		t.Fatal(err)
	}
	owner, err := a.Login(t.Context(), "owner", password, "192.0.2.1", "job admin", "")
	if err != nil {
		t.Fatal(err)
	}
	return NewAdmin(q, a), owner
}

func TestDurableClaimProgressCompletionAndPrivateDTOs(t *testing.T) {
	q, clock := fixture(t)
	job := enqueued(t, q, "first", 0)
	lease := claimed(t, q)
	if lease.Job.ID != job.ID || lease.Job.Status != "running" || lease.Job.Attempts != 1 || lease.Owner == "" {
		t.Fatal("invalid claim", lease)
	}
	payload, _ := json.Marshal(lease.Job)
	for _, secret := range []string{lease.Owner, "not-for-export", "payload_json", "dedupe_token"} {
		if strings.Contains(string(payload), secret) {
			t.Fatal("private job field leaked")
		}
	}
	_, err := q.Progress(t.Context(), lease, Counts{Total: 3, Done: 2, Succeeded: 1}, true)
	requireError(t, err, ErrProgress)
	progress, err := q.Progress(t.Context(), lease, Counts{Total: 3, Done: 1, Succeeded: 1}, true)
	if err != nil || progress.Done != 1 {
		t.Fatal(progress, err)
	}
	requireError(t, q.Complete(t.Context(), lease), ErrProgress)
	*clock = clock.Add(10 * time.Second)
	if err := q.Heartbeat(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	_, err = q.Progress(t.Context(), lease, Counts{Total: 2, Done: 1, Succeeded: 1}, true)
	requireError(t, err, ErrProgress)
	if _, err := q.Progress(t.Context(), lease, Counts{Total: 3, Done: 3, Succeeded: 2, Skipped: 1}, true); err != nil {
		t.Fatal(err)
	}
	if err := q.Complete(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	finished, err := q.Get(t.Context(), job.ID)
	if err != nil || finished.Status != "succeeded" || finished.FinishedAt == nil || finished.Done != 3 || finished.Succeeded != 2 || finished.Skipped != 1 || finished.Version <= progress.Version {
		t.Fatal(finished, err)
	}
	requireError(t, q.Heartbeat(t.Context(), lease), ErrLeaseLost)
	newJob := enqueued(t, q, "first", 0)
	if newJob.ID == job.ID {
		t.Fatal("a terminal job permanently consumed its dedupe key")
	}
	var missing int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE o.id IS NULL`).Scan(&missing); err != nil || missing != 0 {
		t.Fatal("missing job outbox", missing, err)
	}
}

func TestConcurrentClaimIsExclusiveAndPriorityIsRespected(t *testing.T) {
	q, _ := fixture(t)
	job := enqueued(t, q, "only", 1)
	var wg sync.WaitGroup
	results := make(chan Lease, 16)
	failures := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			lease, err := q.Claim(t.Context(), store.NewID(), []string{"scan"})
			if err != nil {
				failures <- err
			} else {
				results <- lease
			}
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	count := 0
	for lease := range results {
		if lease.Job.ID != job.ID {
			t.Fatal("claimed a different job")
		}
		count++
	}
	for err := range failures {
		requireError(t, err, ErrNoWork)
	}
	if count != 1 {
		t.Fatalf("one job claimed %d times", count)
	}
	low, high := enqueued(t, q, "low", 0), enqueued(t, q, "high", 10)
	if got := claimed(t, q); got.Job.ID != high.ID {
		t.Fatal("priority order ignored")
	}
	if got := claimed(t, q); got.Job.ID != low.ID {
		t.Fatal("due low priority job lost")
	}
	_, err := q.Claim(t.Context(), "different-worker", []string{"variant"})
	requireError(t, err, ErrNoWork)
}

func TestRetryPreservesHistoryCountersAndRejectsStaleOwner(t *testing.T) {
	q, clock := fixture(t)
	q.policy.MaxAttempts = 2
	job := enqueued(t, q, "retry", 0)
	first := claimed(t, q)
	if _, err := q.Progress(t.Context(), first, Counts{Total: 10, Done: 4, Succeeded: 3, Failed: 1}, true); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(t.Context(), first, "SOURCE_UNAVAILABLE", true); err != nil {
		t.Fatal(err)
	}
	pending, err := q.Get(t.Context(), job.ID)
	if err != nil || pending.Status != "queued" || pending.Done != 10 || pending.Cancelled != 6 || pending.Failed != 1 {
		t.Fatal("retry reset or invented success counts", pending, err)
	}
	_, err = q.Claim(t.Context(), "worker-two", []string{"scan"})
	requireError(t, err, ErrNoWork)
	*clock = pending.NextRunAt
	second := claimed(t, q)
	if second.Owner == first.Owner || second.Job.Attempts != 2 {
		t.Fatal("lease owner was reused")
	}
	requireError(t, q.Heartbeat(t.Context(), first), ErrLeaseLost)
	requireError(t, q.Complete(t.Context(), first), ErrLeaseLost)
	_, err = q.Progress(t.Context(), first, Counts{}, false)
	requireError(t, err, ErrLeaseLost)
	if _, err := q.Progress(t.Context(), second, Counts{Total: 12, Done: 12, Succeeded: 5, Failed: 1, Cancelled: 6}, true); err != nil {
		t.Fatal(err)
	}
	if err := q.Complete(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	finished, err := q.Get(t.Context(), job.ID)
	if err != nil || finished.Status != "succeeded" || finished.Failed != 1 || finished.Cancelled != 6 {
		t.Fatal("successful retry erased prior work history", finished, err)
	}
	var attempts int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM job_attempts WHERE job_id=?`, job.ID).Scan(&attempts); err != nil || attempts != 2 {
		t.Fatal("attempt history lost", attempts, err)
	}
}

func TestExpiredLeasesRecoverAfterRestartAndExhaustToDeadLetter(t *testing.T) {
	q, clock := fixture(t)
	q.policy.MaxAttempts = 2
	job := enqueued(t, q, "crash", 0)
	old := claimed(t, q)
	*clock = clock.Add(q.policy.Lease)
	requireError(t, q.Heartbeat(t.Context(), old), ErrLeaseLost)
	restarted, err := New(q.db, q.policy)
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = q.now
	_, err = restarted.Claim(t.Context(), "new-process", []string{"scan"})
	requireError(t, err, ErrNoWork)
	pending, err := q.Get(t.Context(), job.ID)
	if err != nil || pending.Status != "queued" || pending.ErrorCode == nil || *pending.ErrorCode != "LEASE_EXPIRED" {
		t.Fatal(pending, err)
	}
	*clock = pending.NextRunAt
	current := claimed(t, restarted)
	requireError(t, q.Complete(t.Context(), old), ErrLeaseLost)
	*clock = clock.Add(q.policy.Lease)
	_, err = restarted.Claim(t.Context(), "third-process", []string{"scan"})
	requireError(t, err, ErrNoWork)
	dead, err := q.Get(t.Context(), job.ID)
	if err != nil || dead.Status != "dead" || dead.Attempts != 2 || dead.FinishedAt == nil {
		t.Fatal("exhausted lease was not dead-lettered", dead, err)
	}
	requireError(t, q.Complete(t.Context(), current), ErrLeaseLost)
	enqueued(t, q, "crash", 0)
}

func TestAdministrativeCancelRetryAndIdempotentReplay(t *testing.T) {
	q, _ := fixture(t)
	admin, owner := adminFixture(t, q)
	job := enqueued(t, q, "cancel", 0)
	key := store.NewID()
	cancelled, err := admin.Cancel(t.Context(), owner, job.ID, job.Version, key)
	if err != nil || cancelled.Status != "cancelled" || !cancelled.CancelRequested {
		t.Fatal(cancelled, err)
	}
	retry, err := admin.Retry(t.Context(), owner, job.ID, cancelled.Version, store.NewID())
	if err != nil || retry.Status != "queued" || retry.CancelRequested {
		t.Fatal(retry, err)
	}
	lease := claimed(t, q)
	oldResult, err := admin.Cancel(t.Context(), owner, job.ID, job.Version, key)
	if err != nil || oldResult.Status != "cancelled" {
		t.Fatal("cancel replay did not return original result", err)
	}
	if err := q.Heartbeat(t.Context(), lease); err != nil {
		t.Fatal("old cancel replay cancelled a retried job", err)
	}
	current, err := admin.Get(t.Context(), owner, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	requested, err := admin.Cancel(t.Context(), owner, job.ID, current.Version, store.NewID())
	if err != nil || requested.Status != "running" || !requested.CancelRequested {
		t.Fatal(requested, err)
	}
	requireError(t, q.Heartbeat(t.Context(), lease), ErrCancelled)
	requireError(t, q.Complete(t.Context(), lease), ErrCancelled)
	if err := q.AcknowledgeCancel(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	attempts, err := admin.Attempts(t.Context(), owner, job.ID, 0, 30)
	if err != nil || len(attempts) != 1 || attempts[0].Status != "cancelled" || attempts[0].FinishedAt == nil {
		t.Fatal("cancel history", attempts, err)
	}
	_, err = admin.Retry(t.Context(), owner, job.ID, 1, store.NewID())
	requireError(t, err, auth.ErrConflict)
}

func TestAdministrativeReadsAndMutationsRecheckCurrentRole(t *testing.T) {
	q, _ := fixture(t)
	admin, owner := adminFixture(t, q)
	job := enqueued(t, q, "private", 0)
	if _, err := q.db.Exec(`UPDATE users SET role='user' WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	checks := []func() error{
		func() error { _, e := admin.Get(t.Context(), owner, job.ID); return e },
		func() error { _, e := admin.List(t.Context(), owner, "", "", 30); return e },
		func() error { _, e := admin.Attempts(t.Context(), owner, job.ID, 0, 30); return e },
		func() error { _, e := admin.Cancel(t.Context(), owner, job.ID, 1, store.NewID()); return e },
		func() error { _, e := admin.Retry(t.Context(), owner, job.ID, 1, store.NewID()); return e },
	}
	for _, check := range checks {
		requireError(t, check(), auth.ErrForbidden)
	}
	if _, err := q.db.Exec(`UPDATE users SET active=0 WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		requireError(t, check(), auth.ErrUnauthenticated)
	}
}

func TestEventAttemptAndReceiptFailuresRollBackWholeTransitions(t *testing.T) {
	q, _ := fixture(t)
	admin, owner := adminFixture(t, q)
	job := enqueued(t, q, "atomic", 0)
	if _, err := q.db.Exec(`CREATE TRIGGER fail_attempt BEFORE INSERT ON job_attempts BEGIN SELECT RAISE(ABORT,'injected attempt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim(t.Context(), "worker", []string{"scan"}); err == nil {
		t.Fatal("claim committed without attempt")
	}
	current, err := q.Get(t.Context(), job.ID)
	if err != nil || current.Status != "queued" || current.Attempts != 0 {
		t.Fatal("partial claim", current, err)
	}
	if _, err := q.db.Exec(`DROP TRIGGER fail_attempt`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.db.Exec(`CREATE TRIGGER fail_receipt BEFORE INSERT ON mutation_receipts BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Cancel(t.Context(), owner, job.ID, 1, store.NewID()); err == nil {
		t.Fatal("cancel committed without replay receipt")
	}
	current, err = q.Get(t.Context(), job.ID)
	if err != nil || current.Status != "queued" || current.CancelRequested {
		t.Fatal("partial cancel", current, err)
	}
	if _, err := q.db.Exec(`DROP TRIGGER fail_receipt`); err != nil {
		t.Fatal(err)
	}
	lease := claimed(t, q)
	if _, err := q.Progress(t.Context(), lease, Counts{Total: 1, Done: 1, Succeeded: 1}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := q.db.Exec(`CREATE TABLE business(id TEXT PRIMARY KEY); CREATE TRIGGER fail_outbox BEFORE INSERT ON outbox_events BEGIN SELECT RAISE(ABORT,'injected outbox failure'); END`); err != nil {
		t.Fatal(err)
	}
	tx, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.RequireLeaseTx(t.Context(), tx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO business(id) VALUES('not-published')`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CompleteTx(t.Context(), tx, lease); err == nil {
		t.Fatal("publish committed without outbox")
	}
	tx.Rollback()
	var count int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM business`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial business publish", count, err)
	}
	current, err = q.Get(t.Context(), job.ID)
	if err != nil || current.Status != "running" {
		t.Fatal("partial completion", current, err)
	}
}

func TestWorkersPolicyValidationAndCancellationBoundaries(t *testing.T) {
	q, clock := fixture(t)
	available, err := q.Available(t.Context())
	if err != nil || available {
		t.Fatal("nonexistent worker reported ready", err)
	}
	if err := q.RegisterWorker(t.Context(), "worker-one", "worker", 2); err != nil {
		t.Fatal(err)
	}
	available, err = q.Available(t.Context())
	if err != nil || !available {
		t.Fatal("registered worker not ready", err)
	}
	*clock = clock.Add(q.policy.Lease)
	available, err = q.Available(t.Context())
	if err != nil || available {
		t.Fatal("stale worker stayed ready", err)
	}
	if err := q.WorkerHeartbeat(t.Context(), "worker-one"); err != nil {
		t.Fatal(err)
	}
	workers, err := q.Workers(t.Context(), "", 1)
	if err != nil || len(workers) != 1 || workers[0].Slots != 2 {
		t.Fatal(workers, err)
	}
	if err := q.StopWorker(t.Context(), "worker-one"); err != nil {
		t.Fatal(err)
	}
	requireError(t, q.WorkerHeartbeat(t.Context(), "worker-one"), ErrState)
	for _, policy := range []Policy{{}, {Concurrency: 17}, func() Policy { p := DefaultPolicy(); p.Heartbeat = p.Lease; return p }()} {
		if _, err := New(q.db, policy); !errors.Is(err, ErrInvalidJob) {
			t.Fatal("invalid worker policy accepted", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = q.Claim(ctx, "worker", []string{"scan"})
	requireError(t, err, context.Canceled)
	if (Counts{Total: 1, Done: 1, Succeeded: 1}).follows(Counts{Total: 2, Done: 1, Succeeded: 1}) {
		t.Fatal("total progress moved backward")
	}
	if (Counts{Total: 1, Done: 1, Succeeded: 1, Failed: 1}).valid() {
		t.Fatal("inconsistent counts accepted")
	}
}

func TestLeaseFenceRejectsPublishingAfterCancellationOrReplacement(t *testing.T) {
	q, clock := fixture(t)
	enqueued(t, q, "fence", 0)
	lease := claimed(t, q)
	*clock = clock.Add(q.policy.Lease)
	tx, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.RequireLeaseTx(t.Context(), tx, lease)
	requireError(t, err, ErrLeaseLost)
	tx.Rollback()
	_, err = q.Get(t.Context(), "missing")
	requireError(t, err, sql.ErrNoRows)
}
