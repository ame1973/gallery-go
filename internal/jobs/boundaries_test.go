package jobs

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"gallerygo/internal/store"
)

func TestEnqueueValidationExplicitDedupeAndAtomicEvents(t *testing.T) {
	q, _ := fixture(t)
	original := enqueued(t, q, "same", 0)
	tx, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	input := Input{Kind: "reprocess", ResourceKey: "source:same", Dedupe: original.Dedupe, Payload: json.RawMessage(`{"mode":"different"}`)}
	duplicate, created, err := q.EnqueueTx(t.Context(), tx, input)
	if err != nil || created || duplicate.ID != original.ID || duplicate.Kind != "scan" {
		t.Fatal("active duplicate was hidden or replaced", duplicate, created, err)
	}
	invalid := []Input{
		{},
		{Kind: "scan", ResourceKey: "/private/path", Dedupe: "valid", Payload: json.RawMessage(`{}`)},
		{Kind: "scan", ResourceKey: "source", Dedupe: "valid", Payload: json.RawMessage(`null`)},
		{Kind: "scan", ResourceKey: "source", Dedupe: "valid", Payload: json.RawMessage(`[]`)},
		{Kind: "scan", ResourceKey: "source", Dedupe: "valid", Payload: json.RawMessage(`{"x":"` + strings.Repeat("x", 16<<10) + `"}`)},
		{Kind: "scan", ResourceKey: "source", Dedupe: "valid", Payload: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}},
		{Kind: "scan", ResourceKey: "source", Dedupe: "valid", Payload: json.RawMessage(`{}`), Priority: 101},
	}
	for _, input := range invalid {
		_, _, err := q.EnqueueTx(t.Context(), tx, input)
		requireError(t, err, ErrInvalidJob)
	}
	tx.Rollback()
	if _, err := q.db.Exec(`CREATE TRIGGER fail_queue_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	tx, err = q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	input.Dedupe = "new-job"
	if _, _, err := q.EnqueueTx(t.Context(), tx, input); err == nil {
		t.Fatal("enqueue committed without audit")
	}
	tx.Rollback()
	var count int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE dedupe_key='new-job'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial enqueue", count, err)
	}
}

func TestFailureResultsAreTruthfulAndDiagnosticsAreBounded(t *testing.T) {
	q, _ := fixture(t)
	job := enqueued(t, q, "bad-media", 0)
	lease := claimed(t, q)
	requireError(t, q.Fail(t.Context(), lease, "/private/source/raw-error", false), ErrInvalidJob)
	requireError(t, q.AcknowledgeCancel(t.Context(), lease), ErrState)
	if _, err := q.Progress(t.Context(), lease, Counts{Total: 2, Done: 2, Succeeded: 1, Failed: 1}, true); err != nil {
		t.Fatal(err)
	}
	if err := q.Complete(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	failed, err := q.Get(t.Context(), job.ID)
	if err != nil || failed.Status != "failed" || failed.Done != 2 || failed.Succeeded != 1 || failed.Failed != 1 || failed.ErrorCode == nil || *failed.ErrorCode != "PROCESSING_FAILED" {
		t.Fatal("done was incorrectly reported as success", failed, err)
	}
	job = enqueued(t, q, "unrecoverable", 0)
	lease = claimed(t, q)
	if _, err := q.Progress(t.Context(), lease, Counts{Total: 3}, true); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(t.Context(), lease, "INVALID_MEDIA", false); err != nil {
		t.Fatal(err)
	}
	failed, err = q.Get(t.Context(), job.ID)
	if err != nil || failed.Status != "failed" || failed.Done != 3 || failed.Succeeded != 0 || failed.Cancelled != 3 {
		t.Fatal("unfinished units invented success", failed, err)
	}
	for _, value := range []string{fmt.Sprint(lease), fmt.Sprint(lease.Job)} {
		if strings.Contains(value, lease.Owner) || strings.Contains(value, "not-for-export") {
			t.Fatal("private capability appeared in normal diagnostics")
		}
	}
	if q.backoff(100) != q.policy.RetryMax {
		t.Fatal("retry backoff exceeded its bound")
	}
	if (Counts{Total: math.MaxInt64, Done: math.MaxInt64, Succeeded: math.MaxInt64, Failed: 1}).valid() || (Counts{Total: 1, Succeeded: -1}).valid() {
		t.Fatal("overflow or negative progress accepted")
	}
}

func TestCancellationRecoveryManualRetryAndBoundedAdminPages(t *testing.T) {
	q, clock := fixture(t)
	admin, owner := adminFixture(t, q)
	job := enqueued(t, q, "stopped-process", 0)
	lease := claimed(t, q)
	progress, err := q.Progress(t.Context(), lease, Counts{Total: 4, Done: 1, Succeeded: 1}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Cancel(t.Context(), owner, job.ID, progress.Version, store.NewID()); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(q.policy.Lease)
	_, err = q.Claim(t.Context(), "after-crash", []string{"scan"})
	requireError(t, err, ErrNoWork)
	cancelled, err := admin.Get(t.Context(), owner, job.ID)
	if err != nil || cancelled.Status != "cancelled" || cancelled.Done != 4 || cancelled.Cancelled != 3 || cancelled.Succeeded != 1 {
		t.Fatal("cancel recovery changed successful work", cancelled, err)
	}
	newJob := enqueued(t, q, "stopped-process", 0)
	_, err = admin.Retry(t.Context(), owner, job.ID, cancelled.Version, store.NewID())
	requireError(t, err, ErrResourceBusy)
	if _, err := admin.Cancel(t.Context(), owner, newJob.ID, newJob.Version, store.NewID()); err != nil {
		t.Fatal(err)
	}
	retried, err := admin.Retry(t.Context(), owner, job.ID, cancelled.Version, store.NewID())
	if err != nil || retried.Attempts != 1 || retried.MaxAttempts != 1+q.policy.MaxAttempts || retried.Done != 4 {
		t.Fatal("manual retry discarded attempts or progress", retried, err)
	}
	items, err := admin.List(t.Context(), owner, "", "queued", 1)
	if err != nil || len(items) != 1 || items[0].ID != job.ID {
		t.Fatal("job status page", items, err)
	}
	items, err = admin.List(t.Context(), owner, "", "", 101)
	if err != nil || len(items) != 2 {
		t.Fatal("bounded job list", items, err)
	}
	for _, limit := range []int{0, 102} {
		_, err := admin.List(t.Context(), owner, "", "", limit)
		requireError(t, err, ErrInvalidJob)
	}
	_, err = admin.List(t.Context(), owner, "", "invented", 30)
	requireError(t, err, ErrInvalidJob)
	_, err = admin.Attempts(t.Context(), owner, "missing", 0, 30)
	requireError(t, err, sql.ErrNoRows)
	_, err = admin.Attempts(t.Context(), owner, job.ID, -1, 30)
	requireError(t, err, ErrInvalidJob)
	_, err = admin.Attempts(t.Context(), owner, job.ID, 0, 102)
	requireError(t, err, ErrInvalidJob)
}
