package library

import (
	"encoding/json"
	"testing"

	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func (f sourceFixture) enqueueParent(t *testing.T, sources ...Source) jobs.Job {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	job, created, err := f.queue.EnqueueTx(t.Context(), tx, jobs.Input{Kind: "scan", ResourceKey: "scan:all", Dedupe: "scan:parent:" + store.NewID(), Payload: json.RawMessage(`{}`), Priority: 10, ActorID: f.owner.User.ID})
	if err != nil || !created {
		t.Fatal(job, created, err)
	}
	for _, source := range sources {
		if err := f.queue.BindResourceTx(t.Context(), tx, job, "source:"+source.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestSourceChangesCancelBoundParentsAndKeepDeletedSourceJobHistory(t *testing.T) {
	f := sourceSetup(t)
	first, second := f.create(t, "photos"), f.create(t, "replacement")
	parent := f.enqueueParent(t, first, second)
	other := f.enqueue(t, second)
	lease, err := f.queue.Claim(t.Context(), "source-parent-worker", []string{"scan"})
	if err != nil || lease.Job.ID != parent.ID {
		t.Fatal(lease, err)
	}
	if _, err := f.queue.Progress(t.Context(), lease, jobs.Counts{Total: 3, Done: 1, Succeeded: 1}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Update(t.Context(), f.owner, first.ID, first.Version, SourcePatch{Active: sourcePointer(false)}); err != nil {
		t.Fatal(err)
	}
	current, err := f.queue.Get(t.Context(), parent.ID)
	if err != nil || current.Status != "running" || !current.CancelRequested || current.Succeeded != 1 || current.Done != 1 {
		t.Fatal("source edit did not atomically invalidate parent", current, err)
	}
	untouched, err := f.queue.Get(t.Context(), other.ID)
	if err != nil || untouched.Status != "queued" || untouched.CancelRequested {
		t.Fatal("other source work was incorrectly cancelled", untouched, err)
	}
	if err := f.service.DeleteEmpty(t.Context(), f.owner, second.ID, second.Version); err != nil {
		t.Fatal(err)
	}
	if err := f.queue.AcknowledgeCancel(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	current, err = f.queue.Get(t.Context(), parent.ID)
	if err != nil || current.Status != "cancelled" || current.Cancelled != 2 || current.Succeeded != 1 {
		t.Fatal("parent cancellation history lost real outcomes", current, err)
	}
	var retained int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM job_resources WHERE job_id=? AND resource_key=?`, parent.ID, "source:"+second.ID).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("source deletion destroyed job dependency history", retained, err)
	}
	untouched, err = f.queue.Get(t.Context(), other.ID)
	if err != nil || untouched.Status != "cancelled" {
		t.Fatal("deleted source left queued work", untouched, err)
	}
}

func TestSourceAuditFailureRollsBackParentCancellationAndConfiguration(t *testing.T) {
	f := sourceSetup(t)
	first, second := f.create(t, "photos"), f.create(t, "replacement")
	parent := f.enqueueParent(t, first, second)
	var scope string
	if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.owner.User.ID).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_bound_source_update BEFORE INSERT ON audit_events WHEN NEW.event_type='source_updated' BEGIN SELECT RAISE(ABORT,'injected final source audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Update(t.Context(), f.owner, second.ID, second.Version, SourcePatch{MinimumFiles: sourcePointer(int64(5))}); err == nil {
		t.Fatal("source edit survived failed audit")
	}
	current, err := f.service.Get(t.Context(), f.owner, second.ID)
	if err != nil || current.ConfigVersion != second.ConfigVersion || current.MinimumFiles != second.MinimumFiles {
		t.Fatal("configuration escaped rollback", current, err)
	}
	job, err := f.queue.Get(t.Context(), parent.ID)
	if err != nil || job.Status != "queued" || job.CancelRequested {
		t.Fatal("parent cancellation escaped rollback", job, err)
	}
	var currentScope string
	if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.owner.User.ID).Scan(&currentScope); err != nil || currentScope != scope {
		t.Fatal("scope rotation escaped rollback", currentScope, err)
	}
}
