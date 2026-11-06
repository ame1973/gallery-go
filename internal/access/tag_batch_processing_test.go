package access

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func claimTagBatch(t *testing.T, b *TagBatches, owner auth.Session, input TagBatchRequest) jobs.Lease {
	t.Helper()
	accepted, err := b.Enqueue(t.Context(), owner, input, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := b.queue.Claim(t.Context(), "batch-unit-worker", []string{"batch_tags"})
	if err != nil || lease.Job.ID != accepted.ID {
		t.Fatal("accepted batch was not claimed", lease.Job, err)
	}
	return lease
}

func TestTagBatchProcessingAppliesRealTagsNoOpsAndCompleteCanonicalEvents(t *testing.T) {
	s, owner, b := tagBatchFixture(t)
	if _, err := s.SetAlbumTags(t.Context(), owner, "batch-local", 2, []string{"batch-tag-a"}, store.NewID()); err != nil {
		t.Fatal(err)
	}
	lease := claimTagBatch(t, b, owner, TagBatchRequest{AlbumIDs: []string{"batch-a", "batch-b", "batch-local"}, TagIDs: []string{"batch-tag-a"}})
	if err := b.Process(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	job, err := b.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Status != "succeeded" || !job.TotalKnown || job.Counts != (jobs.Counts{Total: 3, Done: 3, Succeeded: 2, Skipped: 1}) {
		t.Fatal("processing counters did not describe actual work", job, job.Counts, err)
	}
	results, err := b.Results(t.Context(), owner, job.ID, "", 100)
	if err != nil || len(results) != 3 {
		t.Fatal("committed result history missing", results, err)
	}
	versions := map[string]int64{"batch-a": 8, "batch-b": 4, "batch-local": 3}
	for _, result := range results {
		want := "applied"
		if result.AlbumID == "batch-local" {
			want = "unchanged"
		}
		if result.Outcome != want || result.Attempt != 1 || result.AlbumVersion == nil || *result.AlbumVersion != versions[result.AlbumID] || result.ErrorCode != nil || result.Message != nil {
			t.Fatal("result fabricated a mutation or lost historical version", result)
		}
		actual, err := s.AdminAlbumTags(t.Context(), owner, result.AlbumID)
		if err != nil || actual.Version != versions[result.AlbumID] || !slices.Equal(actual.TagIDs, []string{"batch-tag-a"}) {
			t.Fatal("successful result lacks actual business state", result, actual, err)
		}
		assertAlbumEventTags(t, s, result.AlbumID, []string{"batch-tag-a"})
	}
	before := tagBatchCounts(t, s)
	if err := b.Process(t.Context(), lease); !errors.Is(err, jobs.ErrLeaseLost) || tagBatchCounts(t, s) != before {
		t.Fatal("completed lease republished target state", err)
	}
}

func TestTagBatchSameAttemptResumeNeverAddsBudgetOrDoubleCountsFinalTarget(t *testing.T) {
	s, owner, b := tagBatchFixture(t)
	lease := claimTagBatch(t, b, owner, TagBatchRequest{AlbumIDs: []string{"batch-a", "batch-b"}, TagIDs: []string{"batch-tag-a"}})
	ids, err := b.beginTagBatch(t.Context(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.beginTagBatch(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := b.processTagBatchTarget(t.Context(), lease, id); err != nil {
			t.Fatal(err)
		}
		before := tagBatchCounts(t, s)
		if err := b.processTagBatchTarget(t.Context(), lease, id); err != nil || tagBatchCounts(t, s) != before {
			t.Fatal("same-attempt replay duplicated a resolved target", id, err)
		}
	}
	if err := b.Process(t.Context(), lease); err != nil {
		t.Fatal("resume after last target could not atomically finalize", err)
	}
	job, err := b.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Status != "succeeded" || job.Counts != (jobs.Counts{Total: 2, Done: 2, Succeeded: 2}) {
		t.Fatal("resumption duplicated work budget/results", job, job.Counts, err)
	}
}

func TestTagBatchRetrySkipsPriorResolutionWithoutOverwritingALaterEdit(t *testing.T) {
	s, owner, b := tagBatchFixture(t)
	lease := claimTagBatch(t, b, owner, TagBatchRequest{AlbumIDs: []string{"batch-a", "batch-b", "batch-local"}, TagIDs: []string{"batch-tag-a"}})
	ids, err := b.beginTagBatch(t.Context(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.processTagBatchTarget(t.Context(), lease, ids[0]); err != nil {
		t.Fatal(err)
	}
	later, err := s.SetAlbumTags(t.Context(), owner, "batch-a", 8, []string{"batch-tag-b"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.queue.Fail(t.Context(), lease, "WORKER_STOPPED", false); err != nil {
		t.Fatal(err)
	}
	stopped, err := b.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || stopped.Counts != (jobs.Counts{Total: 3, Done: 3, Succeeded: 1, Cancelled: 2}) {
		t.Fatal("interruption invented completed target work", stopped.Counts, err)
	}
	if _, err := jobs.NewAdmin(b.queue, b.auth).Retry(t.Context(), owner, stopped.ID, stopped.Version, store.NewID()); err != nil {
		t.Fatal(err)
	}
	next, err := b.queue.Claim(t.Context(), "retry-worker", []string{"batch_tags"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.processTagBatchTarget(t.Context(), lease, "batch-b"); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("previous owner survived retry", err)
	}
	if err := b.Process(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	job, err := b.queue.Get(t.Context(), stopped.ID)
	if err != nil || job.Status != "succeeded" || job.Counts != (jobs.Counts{Total: 6, Done: 6, Succeeded: 3, Skipped: 1, Cancelled: 2}) {
		t.Fatal("retry erased history or failed truthful cumulative/attempt semantics", job, job.Counts, err)
	}
	actual, err := s.AdminAlbumTags(t.Context(), owner, "batch-a")
	if err != nil || !reflect.DeepEqual(actual, later) {
		t.Fatal("retry overwrote another later edit", actual, later, err)
	}
	results, err := b.Results(t.Context(), owner, job.ID, "", 100)
	if err != nil || len(results) != 4 {
		t.Fatal("retry lost committed per-attempt results", results, err)
	}
	found := false
	for _, result := range results {
		if result.Attempt == 2 && result.AlbumID == "batch-a" {
			found = true
			if result.Outcome != "already_applied" || result.AlbumVersion == nil || *result.AlbumVersion != 8 {
				t.Fatal("skip was mistaken for the current later version", result)
			}
		}
	}
	if !found {
		t.Fatal("retry omitted the already-applied work unit")
	}
}

func TestTagBatchDurableAuthorityOutlivesLogoutButNotCurrentAccountRevocation(t *testing.T) {
	t.Run("logout", func(t *testing.T) {
		s, owner, b := tagBatchFixture(t)
		lease := claimTagBatch(t, b, owner, TagBatchRequest{AlbumIDs: []string{"batch-a"}, TagIDs: []string{"batch-tag-a"}})
		if err := s.auth.Revoke(t.Context(), owner, owner.ID); err != nil {
			t.Fatal(err)
		}
		if err := b.Process(t.Context(), lease); err != nil {
			t.Fatal("accepted durable work depended on storing/forging a web session", err)
		}
		job, err := b.queue.Get(t.Context(), lease.Job.ID)
		if err != nil || job.Status != "succeeded" || job.Succeeded != 1 {
			t.Fatal("logout erased accepted active-admin intent", job, err)
		}
	})
	for _, operation := range []string{"demote", "disable", "delete"} {
		t.Run(operation, func(t *testing.T) {
			s, owner, b := tagBatchFixture(t)
			lease := claimTagBatch(t, b, owner, TagBatchRequest{AlbumIDs: []string{"batch-a"}, TagIDs: []string{"batch-tag-a"}})
			query := `UPDATE users SET role='user' WHERE id=?`
			if operation == "disable" {
				query = `UPDATE users SET active=0 WHERE id=?`
			}
			if operation == "delete" {
				query = `DELETE FROM users WHERE id=?`
			}
			if _, err := s.db.Exec(query, owner.User.ID); err != nil {
				t.Fatal(err)
			}
			if err := b.Process(t.Context(), lease); err != nil {
				t.Fatal(err)
			}
			job, err := b.queue.Get(t.Context(), lease.Job.ID)
			if err != nil || job.Status != "failed" || job.Counts != (jobs.Counts{Total: 1, Done: 1, Failed: 1}) {
				t.Fatal("revoked original actor still published", job, job.Counts, err)
			}
			var outcome, code string
			var links int
			if err := s.db.QueryRow(`SELECT outcome,error_code,(SELECT COUNT(*) FROM album_tags) FROM album_tag_batch_results WHERE job_id=?`, job.ID).Scan(&outcome, &code, &links); err != nil || outcome != "failed" || code != "ACTOR_NOT_AUTHORIZED" || links != 0 {
				t.Fatal("actor failure lacks safe result/atomic denial", outcome, code, links, err)
			}
		})
	}
}

func TestTagBatchAnotherAdministratorCannotReplaceOriginalActorOnRetry(t *testing.T) {
	s, owner, b := tagBatchFixture(t)
	other := reader(t, s, "batch-retry-admin")
	if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, other.User.ID); err != nil {
		t.Fatal(err)
	}
	lease := claimTagBatch(t, b, owner, TagBatchRequest{AlbumIDs: []string{"batch-a"}, TagIDs: []string{"batch-tag-a"}})
	if _, err := s.db.Exec(`UPDATE users SET role='user' WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if err := b.Process(t.Context(), lease); err != nil {
			t.Fatal(err)
		}
		job, err := b.queue.Get(t.Context(), lease.Job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 3 {
			if job.Status != "succeeded" || job.Counts != (jobs.Counts{Total: 3, Done: 3, Failed: 2, Succeeded: 1}) {
				t.Fatal("current-attempt status/cumulative failures are not truthful", job, job.Counts)
			}
			break
		}
		if job.Status != "failed" || job.Failed != int64(attempt) {
			t.Fatal("retry administrator replaced original authorization", job, job.Counts)
		}
		if attempt == 2 {
			if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, owner.User.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := jobs.NewAdmin(b.queue, b.auth).Retry(t.Context(), other, job.ID, job.Version, store.NewID()); err != nil {
			t.Fatal(err)
		}
		lease, err = b.queue.Claim(t.Context(), "different-admin-retry", []string{"batch_tags"})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestTagBatchTargetAndTagFencesFailIndividuallyWithoutGuessingNewIntent(t *testing.T) {
	for _, query := range []string{
		`UPDATE albums SET version=version+1 WHERE id='batch-a'`,
		`UPDATE albums SET library_id='batch-other' WHERE id='batch-a'`,
		`UPDATE sources SET config_version=config_version+1 WHERE id='batch-source'`,
		`UPDATE sources SET is_active=0 WHERE id='batch-source'`,
		`DELETE FROM tags WHERE id='batch-tag-a'`,
	} {
		t.Run(query, func(t *testing.T) {
			s, owner, b := tagBatchFixture(t)
			lease := claimTagBatch(t, b, owner, TagBatchRequest{AlbumIDs: []string{"batch-a"}, TagIDs: []string{"batch-tag-a"}})
			if _, err := s.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if err := b.Process(t.Context(), lease); err != nil {
				t.Fatal(err)
			}
			job, err := b.queue.Get(t.Context(), lease.Job.ID)
			if err != nil || job.Status != "failed" || job.Failed != 1 || job.Succeeded != 0 {
				t.Fatal("changed target was silently retargeted", job, err)
			}
			results, err := b.Results(t.Context(), owner, job.ID, "", 30)
			want := "TARGET_CHANGED"
			if query == `DELETE FROM tags WHERE id='batch-tag-a'` {
				want = "TAG_NOT_FOUND"
			}
			if err != nil || len(results) != 1 || results[0].ErrorCode == nil || *results[0].ErrorCode != want || results[0].AlbumVersion != nil {
				t.Fatal("changed-target diagnostic not explicit and safe", results, err)
			}
		})
	}
}

func TestTagBatchLateJournalAndAuthorityResourceLeaseFailuresRollBackAllTargetEffects(t *testing.T) {
	s, owner, b := tagBatchFixture(t)
	lease := claimTagBatch(t, b, owner, TagBatchRequest{AlbumIDs: []string{"batch-a", "batch-b"}, TagIDs: []string{"batch-tag-a"}})
	if _, err := b.beginTagBatch(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	if err := b.processTagBatchTarget(t.Context(), lease, "batch-a"); err != nil {
		t.Fatal(err)
	}
	injections := map[string]string{}
	for _, table := range []string{"album_tag_batch_results", "change_log", "outbox_events", "audit_events"} {
		injections[table] = `CREATE TRIGGER reject_batch_target BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'private journal'); END`
	}
	injections["actor"] = `CREATE TRIGGER reject_batch_target AFTER INSERT ON album_tag_batch_results BEGIN UPDATE users SET role='user' WHERE id=(SELECT actor_user_id FROM album_tag_batches WHERE job_id=NEW.job_id); END`
	injections["config"] = `CREATE TRIGGER reject_batch_target AFTER INSERT ON album_tag_batch_results BEGIN UPDATE sources SET config_version=config_version+1 WHERE id='batch-source'; END`
	injections["lease"] = `CREATE TRIGGER reject_batch_target AFTER INSERT ON album_tag_batch_results BEGIN UPDATE jobs SET lease_owner='replaced' WHERE id=NEW.job_id; END`
	injections["expiry"] = `CREATE TRIGGER reject_batch_target AFTER INSERT ON album_tag_batch_results BEGIN UPDATE jobs SET lease_expires_at=0 WHERE id=NEW.job_id; END`
	injections["cancel"] = `CREATE TRIGGER reject_batch_target AFTER INSERT ON album_tag_batch_results BEGIN UPDATE jobs SET cancel_requested=1 WHERE id=NEW.job_id; END`
	for name, query := range injections {
		t.Run(name, func(t *testing.T) {
			before := tagBatchCounts(t, s)
			job, err := b.queue.Get(t.Context(), lease.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := s.db.Exec(`DROP TRIGGER reject_batch_target`); err != nil {
					t.Error(err)
				}
			}()
			if err := b.processTagBatchTarget(t.Context(), lease, "batch-b"); err == nil {
				t.Fatal("late failure/fence change was ignored")
			}
			current, err := b.queue.Get(t.Context(), lease.Job.ID)
			if err != nil || current.Counts != job.Counts || current.Version != job.Version || tagBatchCounts(t, s) != before {
				t.Fatal("target result/progress/business/journal partially committed", current, job, err)
			}
			var recorded int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM album_tag_batch_results WHERE job_id=?`, lease.Job.ID).Scan(&recorded); err != nil || recorded != 1 {
				t.Fatal("failed target left a result without its business/progress", recorded, err)
			}
			actual, err := s.AdminAlbumTags(t.Context(), owner, "batch-b")
			if err != nil || actual.Version != 3 || len(actual.TagIDs) != 0 {
				t.Fatal("late failure retained business metadata", actual, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := b.processTagBatchTarget(ctx, lease, "batch-b"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled work wrote a target result", err)
	}
	if err := b.Process(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
}

func TestTagBatchRunningCancellationKeepsPriorNoOpResolvedWithoutReapplyingLaterEdit(t *testing.T) {
	s, owner, b := tagBatchFixture(t)
	if _, err := s.SetAlbumTags(t.Context(), owner, "batch-a", 7, []string{"batch-tag-a"}, store.NewID()); err != nil {
		t.Fatal(err)
	}
	lease := claimTagBatch(t, b, owner, TagBatchRequest{AlbumIDs: []string{"batch-a", "batch-b", "batch-local"}, TagIDs: []string{"batch-tag-a"}})
	if _, err := b.beginTagBatch(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	if err := b.processTagBatchTarget(t.Context(), lease, "batch-a"); err != nil {
		t.Fatal(err)
	}
	later, err := s.SetAlbumTags(t.Context(), owner, "batch-a", 8, []string{"batch-tag-b"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	current, err := b.queue.Get(t.Context(), lease.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	admin := jobs.NewAdmin(b.queue, b.auth)
	requested, err := admin.Cancel(t.Context(), owner, current.ID, current.Version, store.NewID())
	if err != nil || requested.Status != "running" || !requested.CancelRequested {
		t.Fatal("running cancellation lost cooperative state", requested, err)
	}
	if err := b.Process(t.Context(), lease); !errors.Is(err, jobs.ErrCancelled) {
		t.Fatal("cancelled lease processed another target", err)
	}
	if err := b.queue.AcknowledgeCancel(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	cancelled, err := b.queue.Get(t.Context(), current.ID)
	if err != nil || cancelled.Status != "cancelled" || cancelled.Counts != (jobs.Counts{Total: 3, Done: 3, Skipped: 1, Cancelled: 2}) {
		t.Fatal("cancelled budget invented completed units", cancelled, cancelled.Counts, err)
	}
	if _, err := admin.Retry(t.Context(), owner, cancelled.ID, cancelled.Version, store.NewID()); err != nil {
		t.Fatal(err)
	}
	next, err := b.queue.Claim(t.Context(), "cancel-retry-worker", []string{"batch_tags"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Process(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	job, err := b.queue.Get(t.Context(), current.ID)
	if err != nil || job.Status != "succeeded" || job.Counts != (jobs.Counts{Total: 6, Done: 6, Succeeded: 2, Skipped: 2, Cancelled: 2}) {
		t.Fatal("resolved no-op was reapplied or cancelled history reset", job, job.Counts, err)
	}
	actual, err := s.AdminAlbumTags(t.Context(), owner, "batch-a")
	if err != nil || !reflect.DeepEqual(actual, later) {
		t.Fatal("retry reapplied a resolved no-op over a later edit", actual, later, err)
	}
}

func TestTagBatchWorkBudgetAndFinalizationFailuresRemainReplaySafe(t *testing.T) {
	s, owner, b := tagBatchFixture(t)
	lease := claimTagBatch(t, b, owner, TagBatchRequest{AlbumIDs: []string{"batch-a"}, TagIDs: []string{"batch-tag-a"}})
	for _, table := range []string{"change_log", "outbox_events"} {
		if _, err := s.db.Exec(`CREATE TRIGGER reject_batch_budget BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'private budget failure'); END`); err != nil {
			t.Fatal(err)
		}
		_, beginErr := b.beginTagBatch(t.Context(), lease)
		if _, err := s.db.Exec(`DROP TRIGGER reject_batch_budget`); err != nil {
			t.Fatal(err)
		}
		var runs int
		job, err := b.queue.Get(t.Context(), lease.Job.ID)
		if err != nil || beginErr == nil || job.TotalKnown || job.Counts != (jobs.Counts{}) {
			t.Fatal("failed budget survived progress rollback", beginErr, job, err)
		}
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM album_tag_batch_runs WHERE job_id=?`, job.ID).Scan(&runs); err != nil || runs != 0 {
			t.Fatal("failed budget reserved duplicate retry work", runs, err)
		}
	}
	if _, err := b.beginTagBatch(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	if err := b.processTagBatchTarget(t.Context(), lease, "batch-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_batch_finish BEFORE INSERT ON audit_events WHEN NEW.event_type='tag_batch_finished' BEGIN SELECT RAISE(ABORT,'private finalization failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := b.Process(t.Context(), lease); err == nil {
		t.Fatal("late final audit failure was ignored")
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_batch_finish`); err != nil {
		t.Fatal(err)
	}
	job, err := b.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Status != "running" || job.Counts != (jobs.Counts{Total: 1, Done: 1, Succeeded: 1}) {
		t.Fatal("failed finalization erased committed work or finalized the job", job, err)
	}
	if err := b.Process(t.Context(), lease); err != nil {
		t.Fatal("same-attempt retry could not finalize committed outcomes", err)
	}
	var results, albumEvents int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM album_tag_batch_results WHERE job_id=?),(SELECT COUNT(*) FROM change_log WHERE kind='album' AND resource_id='batch-a')`, job.ID).Scan(&results, &albumEvents); err != nil || results != 1 || albumEvents != 1 {
		t.Fatal("finalization retry duplicated target business/results", results, albumEvents, err)
	}
}

func TestTagBatchConcurrentHandlersDoNotDuplicateBudgetOrLoseCompetingUpdates(t *testing.T) {
	s, owner, b := tagBatchFixture(t)
	run := func(leases []jobs.Lease) {
		t.Helper()
		done := make(chan error, len(leases))
		for _, lease := range leases {
			go func() { done <- b.Process(t.Context(), lease) }()
		}
		for range leases {
			if err := <-done; err != nil && !errors.Is(err, jobs.ErrLeaseLost) {
				t.Error("concurrent batch handler failed unexpectedly", err)
			}
		}
	}
	lease := claimTagBatch(t, b, owner, TagBatchRequest{AlbumIDs: []string{"batch-b"}, TagIDs: []string{"batch-tag-a"}})
	run([]jobs.Lease{lease, lease})
	job, err := b.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Status != "succeeded" || job.Counts != (jobs.Counts{Total: 1, Done: 1, Succeeded: 1}) {
		t.Fatal("duplicate live handler duplicated budget/work", job, job.Counts, err)
	}
	var competing []jobs.Lease
	for _, tag := range []string{"batch-tag-a", "batch-tag-b"} {
		competing = append(competing, claimTagBatch(t, b, owner, TagBatchRequest{AlbumIDs: []string{"batch-a"}, TagIDs: []string{tag}}))
	}
	run(competing)
	var succeeded, failed int
	for _, lease := range competing {
		job, err := b.queue.Get(t.Context(), lease.Job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == "succeeded" && job.Succeeded == 1 {
			succeeded++
		} else if job.Status == "failed" && job.Failed == 1 {
			failed++
		} else {
			t.Fatal("competing intent has an untruthful outcome", job, job.Counts)
		}
	}
	actual, err := s.AdminAlbumTags(t.Context(), owner, "batch-a")
	if err != nil || succeeded != 1 || failed != 1 || actual.Version != 8 || len(actual.TagIDs) != 1 {
		t.Fatal("two frozen versions both overwrote one target", succeeded, failed, actual, err)
	}
}
