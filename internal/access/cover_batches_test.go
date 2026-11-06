package access

import (
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func coverBatchFixture(t *testing.T) (*Service, auth.Session, *AlbumBatches) {
	t.Helper()
	s, owner := albumMetadataFixture(t)
	if _, err := s.db.Exec(`UPDATE albums SET display_name='Preserved display' WHERE id='metadata-album';
 INSERT INTO tags(id,name) VALUES('preserved-cover-tag','Preserved');
 INSERT INTO album_tags(album_id,tag_id) VALUES('metadata-album','preserved-cover-tag');
 INSERT INTO media_revisions(id,media_id,source_sha256,source_size,source_mtime_ns,version,created_at) VALUES('cover-revision','cover-two','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',1,0,1,0);
 UPDATE media SET current_revision_id='cover-revision' WHERE id='cover-two'`); err != nil {
		t.Fatal(err)
	}
	q, err := jobs.New(s.db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return s, owner, NewAlbumBatches(s.db, s.auth, q)
}

func coverBatchCounts(t *testing.T, s *Service) [9]int64 {
	t.Helper()
	var counts [9]int64
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM jobs),(SELECT COUNT(*) FROM album_cover_batches),(SELECT COUNT(*) FROM album_cover_batch_targets),(SELECT COUNT(*) FROM album_cover_batch_runs),(SELECT COUNT(*) FROM album_cover_batch_results),(SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events),(SELECT COUNT(*) FROM mutation_receipts)`).Scan(&counts[0], &counts[1], &counts[2], &counts[3], &counts[4], &counts[5], &counts[6], &counts[7], &counts[8]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func claimCoverBatch(t *testing.T, b *AlbumBatches, owner auth.Session, input CoverBatchRequest) jobs.Lease {
	t.Helper()
	accepted, err := b.EnqueueCovers(t.Context(), owner, input, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := b.queue.Claim(t.Context(), "cover-unit-worker", []string{"batch_covers"})
	if err != nil || lease.Job.ID != accepted.ID {
		t.Fatal(lease.Job, err)
	}
	return lease
}

func TestCoverBatchFreezesTypedProjectionAndAppliesClearWithoutChangingOtherMetadata(t *testing.T) {
	s, owner, b := coverBatchFixture(t)
	input := CoverBatchRequest{Targets: []CoverBatchSelection{{AlbumID: "metadata-empty", CoverMediaID: new("")}, {AlbumID: "metadata-album", CoverMediaID: new("cover-two")}}}
	key := store.NewID()
	accepted, err := b.EnqueueCovers(t.Context(), owner, input, key)
	if err != nil || accepted.Kind != "batch_covers" || accepted.Status != "queued" || accepted.TotalKnown || accepted.Done != 0 {
		t.Fatal("cover enqueue claimed completion", accepted, err)
	}
	targets, err := b.CoverTargets(t.Context(), owner, accepted.ID, "", 101)
	if err != nil || len(targets) != 2 || targets[0].AlbumID != "metadata-album" || targets[0].CoverMediaID != "cover-two" || targets[0].ProjectedCoverID == nil || *targets[0].ProjectedCoverID != "cover-two" || targets[0].MediaVersion == nil || *targets[0].MediaVersion != 1 || targets[0].RevisionID == nil || *targets[0].RevisionID != "cover-revision" || targets[1].ProjectedCoverID != nil || targets[1].MediaVersion != nil || targets[1].RevisionID != nil {
		t.Fatal("cover target did not freeze exact projection/media epoch", targets, err)
	}
	page, err := b.CoverTargets(t.Context(), owner, accepted.ID, targets[0].AlbumID, 1)
	if err != nil || len(page) != 1 || !reflect.DeepEqual(page[0], targets[1]) {
		t.Fatal("cover target keyset lost snapshot", page, err)
	}
	slices.Reverse(input.Targets)
	before := coverBatchCounts(t, s)
	replay, err := b.EnqueueCovers(t.Context(), owner, input, key)
	if err != nil || replay.ID != accepted.ID || coverBatchCounts(t, s) != before {
		t.Fatal("canonical receipt duplicated intent", replay, err)
	}
	coalesced, err := b.EnqueueCovers(t.Context(), owner, input, store.NewID())
	if err != nil || coalesced.ID != accepted.ID {
		t.Fatal("identical active cover snapshots did not coalesce", coalesced, err)
	}
	lease, err := b.queue.Claim(t.Context(), "real-cover-worker", []string{"batch_covers"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Process(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	job, err := b.queue.Get(t.Context(), accepted.ID)
	if err != nil || job.Status != "succeeded" || job.Counts != (jobs.Counts{Total: 2, Done: 2, Succeeded: 1, Skipped: 1}) {
		t.Fatal("cover processing counters are not actual outcomes", job, job.Counts, err)
	}
	actual, err := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album")
	if err != nil || actual.Version != 2 || actual.CoverOverrideID == nil || *actual.CoverOverrideID != "cover-two" || actual.CoverMediaID == nil || *actual.CoverMediaID != "cover-two" || actual.DisplayName == nil || *actual.DisplayName != "Preserved display" {
		t.Fatal("cover batch changed the wrong metadata", actual, err)
	}
	assertAlbumEventTags(t, s, "metadata-album", []string{"preserved-cover-tag"})
	var restricted bool
	var scope string
	if err := s.db.QueryRow(`SELECT a.access_restricted,u.scope_id FROM albums a JOIN users u ON u.id=? WHERE a.id='metadata-album'`, owner.User.ID).Scan(&restricted, &scope); err != nil || !restricted || scope != owner.User.ScopeID {
		t.Fatal("cover edit changed restrictions/authorization", restricted, scope, err)
	}
	clear := claimCoverBatch(t, b, owner, CoverBatchRequest{Targets: []CoverBatchSelection{{AlbumID: "metadata-album", CoverMediaID: new("")}}})
	if err := b.Process(t.Context(), clear); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album")
	if err != nil || cleared.Version != 3 || cleared.CoverOverrideID != nil || cleared.CoverMediaID == nil || *cleared.CoverMediaID != "cover-two" || !sameOverride(cleared.DisplayName, actual.DisplayName) {
		t.Fatal("clear did not preserve sticky current cover and unrelated name", cleared, err)
	}
	replay, err = b.EnqueueCovers(t.Context(), owner, input, key)
	if err != nil || replay.ID != accepted.ID || replay.Status != "queued" {
		t.Fatal("historical accepted result drifted after processing/clear", replay, err)
	}
	history, err := b.CoverTargets(t.Context(), owner, accepted.ID, "", 100)
	if err != nil || !reflect.DeepEqual(history, targets) {
		t.Fatal("cover target history was rewritten", history, err)
	}
	if _, err := b.Targets(t.Context(), owner, accepted.ID, "", 30); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cover intent was mistaken for a tag target set", err)
	}
}

func TestCoverBatchRejectsAmbiguousSelectionAndRechecksOriginalActor(t *testing.T) {
	s, owner, b := coverBatchFixture(t)
	valid := CoverBatchRequest{Targets: []CoverBatchSelection{{AlbumID: "metadata-album", CoverMediaID: new("cover-two")}}}
	before := coverBatchCounts(t, s)
	for _, input := range []CoverBatchRequest{{}, {Targets: []CoverBatchSelection{}}, {Targets: make([]CoverBatchSelection, 101)}, {Targets: []CoverBatchSelection{{AlbumID: "metadata-album"}}}, {Targets: []CoverBatchSelection{{AlbumID: "metadata-album", CoverMediaID: new("cover-two")}, {AlbumID: "metadata-album", CoverMediaID: new("")}}}, {Targets: []CoverBatchSelection{{AlbumID: "metadata-album", CoverMediaID: new(strings.Repeat("x", 65))}}}} {
		if _, err := b.EnqueueCovers(t.Context(), owner, input, store.NewID()); !errors.Is(err, ErrCoverBatchSelection) {
			t.Fatal("invalid cover selection accepted", input, err)
		}
	}
	for _, id := range []string{"cover-foreign", "cover-archived", "cover-not-media", "cover-wrong-source", "missing"} {
		if _, err := b.EnqueueCovers(t.Context(), owner, CoverBatchRequest{Targets: []CoverBatchSelection{{AlbumID: "metadata-album", CoverMediaID: &id}}}, store.NewID()); !errors.Is(err, ErrAlbumCover) {
			t.Fatal("ineligible cover queued", id, err)
		}
	}
	if coverBatchCounts(t, s) != before {
		t.Fatal("invalid requests left queued cover state")
	}
	viewer := reader(t, s, "cover-batch-reader")
	viewer.User.Role = "admin"
	if _, err := b.EnqueueCovers(t.Context(), viewer, valid, store.NewID()); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("forged caller role authorized a cover batch", err)
	}
	lease := claimCoverBatch(t, b, owner, valid)
	if _, err := s.db.Exec(`UPDATE users SET role='user' WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.Process(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	job, err := b.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Status != "failed" || job.Failed != 1 || job.Succeeded != 0 {
		t.Fatal("revoked original actor still changed cover", job, err)
	}
}

func TestCoverBatchRejectsMediaRewriteMoveArchiveAndChangedClearProjection(t *testing.T) {
	for _, query := range []string{
		`UPDATE media SET version=version+1 WHERE id='cover-two'`,
		`INSERT INTO media_revisions(id,media_id,source_sha256,source_size,source_mtime_ns,version,created_at) VALUES('cover-rewrite','cover-two','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',1,0,2,0); UPDATE media SET current_revision_id='cover-rewrite' WHERE id='cover-two'`,
		`UPDATE media SET album_id='metadata-other' WHERE id='cover-two'`,
		`UPDATE media SET availability='archived' WHERE id='cover-two'`,
		`UPDATE media SET metadata_json='{"status":"not_media"}' WHERE id='cover-two'`,
	} {
		t.Run(query, func(t *testing.T) {
			s, owner, b := coverBatchFixture(t)
			lease := claimCoverBatch(t, b, owner, CoverBatchRequest{Targets: []CoverBatchSelection{{AlbumID: "metadata-album", CoverMediaID: new("cover-two")}}})
			if _, err := s.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if err := b.Process(t.Context(), lease); err != nil {
				t.Fatal(err)
			}
			results, err := b.Results(t.Context(), owner, lease.Job.ID, "", 30)
			if err != nil || len(results) != 1 || results[0].Outcome != "failed" || results[0].ErrorCode == nil || *results[0].ErrorCode != "COVER_CHANGED" || results[0].AlbumVersion != nil {
				t.Fatal("changed cover was silently retargeted", results, err)
			}
			actual, err := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album")
			if err != nil || actual.Version != 1 || actual.CoverOverrideID != nil {
				t.Fatal("cover rejection partially changed album", actual, err)
			}
		})
	}
	t.Run("clear", func(t *testing.T) {
		s, owner, b := coverBatchFixture(t)
		lease := claimCoverBatch(t, b, owner, CoverBatchRequest{Targets: []CoverBatchSelection{{AlbumID: "metadata-empty", CoverMediaID: new("")}}})
		if _, err := s.db.Exec(`INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES('new-empty-cover','metadata-empty','image','new.jpg',0,0)`); err != nil {
			t.Fatal(err)
		}
		if err := b.Process(t.Context(), lease); err != nil {
			t.Fatal(err)
		}
		results, err := b.Results(t.Context(), owner, lease.Job.ID, "", 30)
		if err != nil || len(results) != 1 || results[0].ErrorCode == nil || *results[0].ErrorCode != "COVER_CHANGED" {
			t.Fatal("clear silently selected an unreviewed new fallback", results, err)
		}
	})
}

func TestCoverBatchLateMediaAndJournalFailureRollsBackBusinessResultAndProgress(t *testing.T) {
	s, owner, b := coverBatchFixture(t)
	lease := claimCoverBatch(t, b, owner, CoverBatchRequest{Targets: []CoverBatchSelection{{AlbumID: "metadata-album", CoverMediaID: new("cover-two")}}})
	if _, err := b.beginTagBatch(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	injections := []string{
		`CREATE TRIGGER reject_cover_target AFTER INSERT ON album_cover_batch_results BEGIN UPDATE media SET version=version+1 WHERE id='cover-two'; END`,
		`CREATE TRIGGER reject_cover_target BEFORE INSERT ON album_cover_batch_results BEGIN SELECT RAISE(ABORT,'private cover result'); END`,
		`CREATE TRIGGER reject_cover_target BEFORE INSERT ON outbox_events BEGIN SELECT RAISE(ABORT,'private cover outbox'); END`,
		`CREATE TRIGGER reject_cover_target BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'private cover audit'); END`,
		`CREATE TRIGGER reject_cover_target AFTER INSERT ON album_cover_batch_results BEGIN UPDATE jobs SET lease_owner='replaced' WHERE id=NEW.job_id; END`,
	}
	for _, query := range injections {
		before := coverBatchCounts(t, s)
		if _, err := s.db.Exec(query); err != nil {
			t.Fatal(err)
		}
		err := b.processTagBatchTarget(t.Context(), lease, "metadata-album")
		if _, dropErr := s.db.Exec(`DROP TRIGGER reject_cover_target`); dropErr != nil {
			t.Fatal(dropErr)
		}
		if err == nil || coverBatchCounts(t, s) != before {
			t.Fatal("late cover failure partially committed", err, before, coverBatchCounts(t, s))
		}
		actual, err := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album")
		if err != nil || actual.Version != 1 || actual.CoverOverrideID != nil || actual.CoverMediaID == nil || *actual.CoverMediaID != "cover-one" {
			t.Fatal("late cover failure retained metadata", actual, err)
		}
	}
	if err := b.Process(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
}
