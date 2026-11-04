package jobs

import (
	"database/sql"
	"encoding/json"
	"testing"
)

func TestResourceBindingsArePrivateIdempotentAndCreationTransactionOnly(t *testing.T) {
	q, _ := fixture(t)
	tx, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	input := Input{Kind: "scan", ResourceKey: "scan:all", Dedupe: "scan:resources", Payload: json.RawMessage(`{}`)}
	job, created, err := q.EnqueueTx(t.Context(), tx, input)
	if err != nil || !created {
		t.Fatal(job, created, err)
	}
	for _, resource := range []string{"source:one", "source:two", "source:one", "scan:all"} {
		if err := q.BindResourceTx(t.Context(), tx, job, resource); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM job_resources WHERE job_id=?`, job.ID).Scan(&count); err != nil || count != 3 {
		t.Fatal("duplicate resource or missing primary", count, err)
	}
	duplicate, created, err := q.EnqueueTx(t.Context(), tx, input)
	if err != nil || created || duplicate.ID != job.ID {
		t.Fatal(duplicate, created, err)
	}
	requireError(t, q.BindResourceTx(t.Context(), tx, duplicate, "source:unrequested"), ErrState)
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var publicCopy Job
	if err := json.Unmarshal(data, &publicCopy); err != nil {
		t.Fatal(err)
	}
	requireError(t, q.BindResourceTx(t.Context(), tx, publicCopy, "source:unrequested"), ErrState)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	requireError(t, q.BindResourceTx(t.Context(), tx, job, "source:late"), sql.ErrTxDone)
	next, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Rollback()
	requireError(t, q.BindResourceTx(t.Context(), next, job, "source:late"), ErrState)
	if err := next.QueryRow(`SELECT COUNT(*) FROM job_resources WHERE job_id=?`, job.ID).Scan(&count); err != nil || count != 3 {
		t.Fatal("committed resource set expanded", count, err)
	}
}

func TestResourceBindingFailureRollsBackJobDependenciesAndEvents(t *testing.T) {
	q, _ := fixture(t)
	if _, err := q.db.Exec(`CREATE TRIGGER fail_resource_binding BEFORE INSERT ON job_resources WHEN NEW.resource_key='source:failing' BEGIN SELECT RAISE(ABORT,'injected later resource failure'); END`); err != nil {
		t.Fatal(err)
	}
	tx, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	job, created, err := q.EnqueueTx(t.Context(), tx, Input{Kind: "scan", ResourceKey: "scan:all", Dedupe: "scan:atomic-resources", Payload: json.RawMessage(`{}`)})
	if err != nil || !created {
		t.Fatal(job, created, err)
	}
	if err := q.BindResourceTx(t.Context(), tx, job, "source:one"); err != nil {
		t.Fatal(err)
	}
	if err := q.BindResourceTx(t.Context(), tx, job, "source:failing"); err == nil {
		t.Fatal("later binding failure ignored")
	}
	tx.Rollback()
	var count int
	if err := q.db.QueryRow(`SELECT (SELECT COUNT(*) FROM jobs)+(SELECT COUNT(*) FROM job_resources)+(SELECT COUNT(*) FROM change_log)+(SELECT COUNT(*) FROM outbox_events)+(SELECT COUNT(*) FROM audit_events)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial job/target/event creation", count, err)
	}
}

func TestResourceBindingRejectsInvalidOrChangedNewJobs(t *testing.T) {
	q, _ := fixture(t)
	tx, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	job, _, err := q.EnqueueTx(t.Context(), tx, Input{Kind: "scan", ResourceKey: "scan:all", Dedupe: "scan:boundaries", Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"", "/private/path", "source:bad\x00value"} {
		requireError(t, q.BindResourceTx(t.Context(), tx, job, resource), ErrInvalidJob)
	}
	requireError(t, q.BindResourceTx(t.Context(), nil, job, "source:one"), ErrInvalidJob)
	if _, err := tx.Exec(`UPDATE jobs SET version=2 WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	requireError(t, q.BindResourceTx(t.Context(), tx, job, "source:one"), ErrState)
}

func TestAnyBoundSourceCancellationPreservesOtherJobsAndDependencies(t *testing.T) {
	q, _ := fixture(t)
	tx, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := q.EnqueueTx(t.Context(), tx, Input{Kind: "scan", ResourceKey: "scan:all", Dedupe: "scan:parent", Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"source:one", "source:two"} {
		if err := q.BindResourceTx(t.Context(), tx, parent, resource); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	other := enqueued(t, q, "other", -1)
	lease := claimed(t, q)
	if _, err := q.Progress(t.Context(), lease, Counts{Total: 3, Done: 1, Succeeded: 1}, true); err != nil {
		t.Fatal(err)
	}
	tx, err = q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	count, err := q.CancelResourceTx(t.Context(), tx, "source:two", "configuration-admin")
	if err != nil || count != 1 {
		t.Fatal("secondary source failed to cancel parent", count, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err := q.Get(t.Context(), parent.ID)
	if err != nil || current.Status != "running" || !current.CancelRequested || current.Done != 1 || current.Succeeded != 1 {
		t.Fatal("parent cancellation invented completion", current, err)
	}
	requireError(t, q.Heartbeat(t.Context(), lease), ErrCancelled)
	if err := q.AcknowledgeCancel(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	current, err = q.Get(t.Context(), parent.ID)
	if err != nil || current.Status != "cancelled" || current.Cancelled != 2 {
		t.Fatal(current, err)
	}
	untouched, err := q.Get(t.Context(), other.ID)
	if err != nil || untouched.CancelRequested || untouched.Status != "queued" {
		t.Fatal("unrelated job changed", untouched, err)
	}
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM job_resources WHERE job_id=?`, parent.ID).Scan(&count); err != nil || count != 3 {
		t.Fatal("terminal history lost resource bindings", count, err)
	}
}
