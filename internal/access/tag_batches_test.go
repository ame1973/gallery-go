package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func tagBatchFixture(t *testing.T) (*Service, auth.Session, *TagBatches) {
	t.Helper()
	s, owner := fixture(t)
	if _, err := s.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('batch-library','Batch',0),('batch-other','Other',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('batch-source','batch-library','Batch','/private/never-open-batch-source','{}','unavailable',0);
 INSERT INTO albums(id,library_id,source_id,source_name,version,access_restricted,created_at,updated_at) VALUES
 ('batch-a','batch-library','batch-source','A',7,1,0,0),('batch-b','batch-library','batch-source','B',3,0,0,0),
 ('batch-local','batch-library',NULL,'Local',2,0,0,0),('batch-inconsistent','batch-other','batch-source','Mismatch',1,0,0,0);
 INSERT INTO tags(id,name) VALUES('batch-tag-a','First'),('batch-tag-b','Second')`); err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(s.db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return s, owner, NewTagBatches(s.db, s.auth, queue)
}

func tagBatchCounts(t *testing.T, s *Service) [9]int64 {
	t.Helper()
	var counts [9]int64
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM jobs),(SELECT COUNT(*) FROM album_tag_batches),
 (SELECT COUNT(*) FROM album_tag_batch_targets),(SELECT COUNT(*) FROM job_resources),(SELECT COUNT(*) FROM change_log),
 (SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events),(SELECT COUNT(*) FROM mutation_receipts),(SELECT COUNT(*) FROM album_tags)`).Scan(
		&counts[0], &counts[1], &counts[2], &counts[3], &counts[4], &counts[5], &counts[6], &counts[7], &counts[8]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestTagBatchEnqueueFreezesBoundedIntentAndCanonicalHistoricalReceipt(t *testing.T) {
	s, owner, batches := tagBatchFixture(t)
	before := tagBatchCounts(t, s)
	key := store.NewID()
	input := TagBatchRequest{AlbumIDs: []string{"batch-local", "batch-b", "batch-a"}, TagIDs: []string{"batch-tag-b", "batch-tag-a"}}
	job, err := batches.Enqueue(t.Context(), owner, input, key)
	if err != nil || job.Kind != "batch_tags" || job.Status != "queued" || job.Attempts != 0 || job.TotalKnown || job.Counts != (jobs.Counts{}) || job.Version != 1 {
		t.Fatal("acceptance was not a new queued intent", job, err)
	}
	if input.AlbumIDs[0] != "batch-local" || input.TagIDs[0] != "batch-tag-b" {
		t.Fatal("canonicalization mutated caller-owned arrays", input)
	}
	after := tagBatchCounts(t, s)
	want := before
	for index, delta := range [9]int64{1, 1, 3, 5, 1, 1, 2, 1, 0} {
		want[index] += delta
	}
	if after != want {
		t.Fatal("job/intent/targets/dependencies/change/outbox/audits/receipt did not co-commit", before, after, want)
	}
	targets, err := batches.Targets(t.Context(), owner, job.ID, "", 101)
	if err != nil || len(targets) != 3 || targets[0].AlbumID != "batch-a" || targets[0].AlbumVersion != 7 || targets[0].LibraryID != "batch-library" || targets[0].SourceID == nil || *targets[0].SourceID != "batch-source" || targets[0].SourceConfigVersion == nil || *targets[0].SourceConfigVersion != 1 || targets[2].SourceID != nil || targets[2].SourceConfigVersion != nil || !slices.Equal(targets[0].TagIDs, []string{"batch-tag-a", "batch-tag-b"}) {
		t.Fatal("target snapshot lost exact resource/config epochs or explicit tag set", targets, err)
	}
	page, err := batches.Targets(t.Context(), owner, job.ID, targets[0].AlbumID, 1)
	if err != nil || len(page) != 1 || !reflect.DeepEqual(page[0], targets[1]) {
		t.Fatal("target keysets lost frozen selection", page, err)
	}
	slices.Reverse(input.AlbumIDs)
	slices.Reverse(input.TagIDs)
	replay, err := batches.Enqueue(t.Context(), owner, input, key)
	if err != nil || replay.ID != job.ID || replay.Version != job.Version || tagBatchCounts(t, s) != after {
		t.Fatal("canonical receipt replay changed the accepted result", replay, err)
	}
	coalesced, err := batches.Enqueue(t.Context(), owner, input, store.NewID())
	if err != nil || coalesced.ID != job.ID {
		t.Fatal("identical active frozen intent did not coalesce", coalesced, err)
	}
	coalescedCounts := tagBatchCounts(t, s)
	if coalescedCounts[0] != after[0] || coalescedCounts[2] != after[2] || coalescedCounts[3] != after[3] || coalescedCounts[4] != after[4] || coalescedCounts[6] != after[6]+1 || coalescedCounts[7] != after[7]+1 {
		t.Fatal("coalescing expanded targets/bindings or duplicated job events", after, coalescedCounts)
	}
	var version int64
	var restricted bool
	if err := s.db.QueryRow(`SELECT version,access_restricted FROM albums WHERE id='batch-a'`).Scan(&version, &restricted); err != nil || version != 7 || !restricted || coalescedCounts[8] != 0 {
		t.Fatal("enqueue applied metadata or weakened album policy", version, restricted, err)
	}
	if _, err := batches.queue.Claim(t.Context(), "scan-only-test", []string{"scan"}); !errors.Is(err, jobs.ErrNoWork) {
		t.Fatal("scan-only kind selection claimed a batch job", err)
	}
	if _, err := batches.Enqueue(t.Context(), owner, TagBatchRequest{AlbumIDs: []string{"batch-a"}, TagIDs: input.TagIDs}, key); !errors.Is(err, store.ErrIdempotencyReuse) {
		t.Fatal("receipt accepted a different target selection", err)
	}
	// Historical acceptance precedes current resource lookup; it must not retarget
	// after indexed metadata/configuration or tag-catalog changes.
	if _, err := s.db.Exec(`UPDATE albums SET version=version+1 WHERE id='batch-a';
 UPDATE sources SET config_version=config_version+1 WHERE id='batch-source';
 DELETE FROM tags WHERE id='batch-tag-b'`); err != nil {
		t.Fatal(err)
	}
	replay, err = batches.Enqueue(t.Context(), owner, input, key)
	if err != nil || replay.ID != job.ID || replay.Version != 1 {
		t.Fatal("historical receipt was revalidated as a new intent", replay, err)
	}
	still, err := batches.Targets(t.Context(), owner, job.ID, "", 100)
	if err != nil || !reflect.DeepEqual(still, targets) {
		t.Fatal("accepted targets drifted with current resources", still, targets, err)
	}
	data, _ := json.Marshal(still)
	for _, private := range []string{"never-open", "relative_path", "password", "lease_owner", "scope_id"} {
		if strings.Contains(string(data), private) {
			t.Fatal("target DTO exposed private authority or paths", string(data))
		}
	}
}

func TestTagBatchDifferentTargetsVersionsEpochsActorsAndTerminalJobsDoNotCoalesce(t *testing.T) {
	s, owner, batches := tagBatchFixture(t)
	input := TagBatchRequest{AlbumIDs: []string{"batch-a"}, TagIDs: []string{"batch-tag-a"}}
	seen := map[string]bool{}
	enqueue := func(actor auth.Session, request TagBatchRequest) jobs.Job {
		t.Helper()
		job, err := batches.Enqueue(t.Context(), actor, request, store.NewID())
		if err != nil || seen[job.ID] {
			t.Fatal("distinct frozen intent reused another active job", job, err)
		}
		seen[job.ID] = true
		return job
	}
	first := enqueue(owner, input)
	enqueue(owner, TagBatchRequest{AlbumIDs: []string{"batch-a", "batch-b"}, TagIDs: input.TagIDs})
	enqueue(owner, TagBatchRequest{AlbumIDs: input.AlbumIDs, TagIDs: []string{}})
	for _, query := range []string{`UPDATE albums SET version=version+1 WHERE id='batch-a'`, `UPDATE sources SET config_version=config_version+1 WHERE id='batch-source'`} {
		if _, err := s.db.Exec(query); err != nil {
			t.Fatal(err)
		}
		enqueue(owner, input)
	}
	other := reader(t, s, "batch-other-admin")
	if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, other.User.ID); err != nil {
		t.Fatal(err)
	}
	enqueue(other, input)
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := batches.queue.CancelResourceTx(t.Context(), tx, "source:batch-source", owner.User.ID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err := batches.queue.Get(t.Context(), first.ID)
	if err != nil || current.Status != "cancelled" || current.Attempts != 0 || current.Succeeded != 0 {
		t.Fatal("source dependency did not cancel queued intent truthfully", current, err)
	}
	enqueue(owner, input)
}

func TestTagBatchSelectionValidationAndFreshAdministrativeBoundary(t *testing.T) {
	s, owner, batches := tagBatchFixture(t)
	valid := TagBatchRequest{AlbumIDs: []string{"batch-a"}, TagIDs: []string{}}
	before := tagBatchCounts(t, s)
	for _, input := range []TagBatchRequest{
		{}, {AlbumIDs: []string{}, TagIDs: []string{}}, {AlbumIDs: valid.AlbumIDs},
		{AlbumIDs: []string{"batch-a", "batch-a"}, TagIDs: []string{}},
		{AlbumIDs: []string{""}, TagIDs: []string{}}, {AlbumIDs: make([]string, 101), TagIDs: []string{}},
		{AlbumIDs: valid.AlbumIDs, TagIDs: make([]string, 101)},
		{AlbumIDs: valid.AlbumIDs, TagIDs: []string{"batch-tag-a", "batch-tag-a"}},
		{AlbumIDs: []string{strings.Repeat("x", 65)}, TagIDs: []string{}},
		{AlbumIDs: valid.AlbumIDs, TagIDs: []string{"bad\x00id"}},
	} {
		if _, err := batches.Enqueue(t.Context(), owner, input, store.NewID()); !errors.Is(err, ErrTagBatchSelection) {
			t.Fatal("invalid or unbounded selection was accepted", input, err)
		}
	}
	if _, err := batches.Enqueue(t.Context(), owner, TagBatchRequest{AlbumIDs: valid.AlbumIDs, TagIDs: []string{"unknown"}}, store.NewID()); !errors.Is(err, ErrUnknownTag) {
		t.Fatal("unknown desired tag was accepted", err)
	}
	for _, id := range []string{"missing", "batch-inconsistent"} {
		if _, err := batches.Enqueue(t.Context(), owner, TagBatchRequest{AlbumIDs: []string{id}, TagIDs: []string{}}, store.NewID()); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("missing/inconsistent album became a target", id, err)
		}
	}
	if _, err := batches.Enqueue(t.Context(), owner, valid, ""); !errors.Is(err, store.ErrIdempotencyKey) {
		t.Fatal("enqueue accepted an implicit receipt key", err)
	}
	if tagBatchCounts(t, s) != before {
		t.Fatal("invalid selection left partial queued intent")
	}
	viewer := reader(t, s, "batch-reader")
	forged := viewer
	forged.User.Role = "admin"
	for _, actor := range []auth.Session{viewer, forged} {
		if _, err := batches.Enqueue(t.Context(), actor, valid, store.NewID()); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller role authorized batch intent", err)
		}
		if _, err := batches.Targets(t.Context(), actor, "missing", "", 30); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller role exposed batch history", err)
		}
	}
	key := store.NewID()
	if _, err := batches.Enqueue(t.Context(), owner, valid, key); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`UPDATE libraries SET is_active=0 WHERE id='batch-library'`, `UPDATE libraries SET is_active=1 WHERE id='batch-library'; UPDATE sources SET is_active=0 WHERE id='batch-source'`} {
		if _, err := s.db.Exec(query); err != nil {
			t.Fatal(err)
		}
		if _, err := batches.Enqueue(t.Context(), owner, valid, store.NewID()); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("inactive resource accepted new intent", err)
		}
	}
	if _, err := s.db.Exec(`UPDATE users SET role='user' WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := batches.Enqueue(t.Context(), owner, valid, key); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("historical receipt bypassed current administrator authority", err)
	}
	for _, limit := range []int{0, 102} {
		if _, err := batches.Targets(t.Context(), owner, "missing", "", limit); !errors.Is(err, ErrTagBatchSelection) {
			t.Fatal("unbounded target page was accepted", limit, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := batches.Enqueue(ctx, owner, valid, store.NewID()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled enqueue proceeded", err)
	}
}

func TestTagBatchConcurrentReceiptsAndLateFailuresRemainAtomic(t *testing.T) {
	s, owner, batches := tagBatchFixture(t)
	input := TagBatchRequest{AlbumIDs: []string{"batch-b", "batch-a"}, TagIDs: []string{"batch-tag-a"}}
	for _, table := range []string{"album_tag_batches", "album_tag_batch_targets", "job_resources", "change_log", "outbox_events", "audit_events", "mutation_receipts"} {
		t.Run(table, func(t *testing.T) {
			before := tagBatchCounts(t, s)
			if _, err := s.db.Exec(`CREATE TRIGGER reject_batch_journal BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'private batch failure'); END`); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := s.db.Exec(`DROP TRIGGER reject_batch_journal`); err != nil {
					t.Error(err)
				}
			}()
			if _, err := batches.Enqueue(t.Context(), owner, input, store.NewID()); err == nil || tagBatchCounts(t, s) != before {
				t.Fatal("late failure left job/target/dependency/journal state", err)
			}
		})
	}
	before := tagBatchCounts(t, s)
	if _, err := s.db.Exec(`CREATE TRIGGER revoke_batch_actor AFTER INSERT ON album_tag_batches BEGIN UPDATE users SET role='user' WHERE id=NEW.actor_user_id; END`); err != nil {
		t.Fatal(err)
	}
	if _, err := batches.Enqueue(t.Context(), owner, input, store.NewID()); !errors.Is(err, auth.ErrForbidden) || tagBatchCounts(t, s) != before {
		t.Fatal("late authorization loss was not rolled back with intent", err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER revoke_batch_actor`); err != nil {
		t.Fatal(err)
	}
	key := store.NewID()
	type outcome struct {
		job jobs.Job
		err error
	}
	results := make(chan outcome, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			job, err := batches.Enqueue(t.Context(), owner, input, key)
			results <- outcome{job, err}
		})
	}
	group.Wait()
	close(results)
	var id string
	for result := range results {
		if result.err != nil || (id != "" && result.job.ID != id) {
			t.Fatal("same-key writers created different intents", result.job, result.err)
		}
		id = result.job.ID
	}
	after := tagBatchCounts(t, s)
	if after[0] != before[0]+1 || after[1] != before[1]+1 || after[2] != before[2]+2 || after[7] != before[7]+1 {
		t.Fatal("concurrent receipt did not commit exactly once", before, after)
	}
}

func TestTagBatchMaximumSelectionKeysetsAndHistorySurviveResourceActorRemoval(t *testing.T) {
	s, owner, batches := tagBatchFixture(t)
	ids := make([]string, 100)
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range ids {
		ids[index] = fmt.Sprintf("batch-page-%03d", index)
		if _, err := tx.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES(?,'batch-library','batch-source','Paged',0,0)`, ids[index]); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	job, err := batches.Enqueue(t.Context(), owner, TagBatchRequest{AlbumIDs: ids, TagIDs: []string{}}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	var observed []string
	for after := ""; ; {
		page, err := batches.Targets(t.Context(), owner, job.ID, after, 17)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, target := range page {
			if target.TagIDs == nil || len(target.TagIDs) != 0 {
				t.Fatal("explicit clear became null", target)
			}
			observed = append(observed, target.AlbumID)
		}
		after = page[len(page)-1].AlbumID
	}
	if !slices.Equal(observed, ids) {
		t.Fatal("bounded keysets duplicated or omitted frozen targets", observed)
	}
	other := reader(t, s, "history-admin")
	if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, other.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM albums; DELETE FROM sources`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM users WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	history, err := batches.Targets(t.Context(), other, job.ID, "", 100)
	if err != nil || len(history) != 100 || history[0].SourceID == nil || *history[0].SourceID != "batch-source" || history[0].SourceConfigVersion == nil || *history[0].SourceConfigVersion != 1 {
		t.Fatal("resource/account removal erased accepted target history", len(history), err)
	}
	var actor string
	if err := s.db.QueryRow(`SELECT actor_user_id FROM album_tag_batches WHERE job_id=?`, job.ID).Scan(&actor); err != nil || actor != owner.User.ID {
		t.Fatal("historical actor was lost or retargeted", actor, err)
	}
	if _, err := batches.Targets(t.Context(), other, "missing", "", 30); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("unknown batch invented empty history", err)
	}
}
