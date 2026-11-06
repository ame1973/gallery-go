package access

import (
	"reflect"
	"testing"

	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func TestCoverBatchInterruptedRetryNeverOverwritesLaterMetadataOrRewrittenMedia(t *testing.T) {
	s, owner, b := coverBatchFixture(t)
	lease := claimCoverBatch(t, b, owner, CoverBatchRequest{Targets: []CoverBatchSelection{{AlbumID: "metadata-album", CoverMediaID: new("cover-two")}, {AlbumID: "metadata-empty", CoverMediaID: new("")}}})
	if _, err := b.beginTagBatch(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	if err := b.processTagBatchTarget(t.Context(), lease, "metadata-album"); err != nil {
		t.Fatal(err)
	}
	later, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 2, AlbumMetadataPatch{CoverMediaID: new("cover-one"), DisplayName: new("Later display")}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE media SET version=version+1 WHERE id='cover-two'`); err != nil {
		t.Fatal(err)
	}
	if err := b.queue.Fail(t.Context(), lease, "WORKER_STOPPED", false); err != nil {
		t.Fatal(err)
	}
	stopped, err := b.queue.Get(t.Context(), lease.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.NewAdmin(b.queue, b.auth).Retry(t.Context(), owner, stopped.ID, stopped.Version, store.NewID()); err != nil {
		t.Fatal(err)
	}
	next, err := b.queue.Claim(t.Context(), "cover-retry-worker", []string{"batch_covers"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Process(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	job, err := b.queue.Get(t.Context(), stopped.ID)
	if err != nil || job.Status != "succeeded" || job.Counts != (jobs.Counts{Total: 4, Done: 4, Succeeded: 1, Skipped: 2, Cancelled: 1}) {
		t.Fatal("cover retry lost truthful cumulative history", job, job.Counts, err)
	}
	actual, err := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album")
	if err != nil || !reflect.DeepEqual(actual, later) {
		t.Fatal("resolved cover retry overwrote a later edit", actual, later, err)
	}
	results, err := b.Results(t.Context(), owner, job.ID, "", 100)
	if err != nil || len(results) != 3 {
		t.Fatal("cover retry lost outcome history", results, err)
	}
	for _, result := range results {
		if result.Attempt == 2 && result.AlbumID == "metadata-album" && (result.Outcome != "already_applied" || result.AlbumVersion == nil || *result.AlbumVersion != 2) {
			t.Fatal("already-applied cover pretended to be a current media version", result)
		}
	}
}

func TestCoverAndTagIntentsRemainDistinctAndFenceCompetingAlbumVersions(t *testing.T) {
	s, owner, b := coverBatchFixture(t)
	cover, err := b.EnqueueCovers(t.Context(), owner, CoverBatchRequest{Targets: []CoverBatchSelection{{AlbumID: "metadata-album", CoverMediaID: new("cover-two")}}}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	tag, err := b.Enqueue(t.Context(), owner, TagBatchRequest{AlbumIDs: []string{"metadata-album"}, TagIDs: []string{}}, store.NewID())
	if err != nil || tag.ID == cover.ID {
		t.Fatal("distinct operation kinds coalesced", tag, cover, err)
	}
	first, err := b.queue.Claim(t.Context(), "tag-before-cover", []string{"batch_tags"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Process(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second, err := b.queue.Claim(t.Context(), "cover-after-tag", []string{"batch_covers"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Process(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	results, err := b.Results(t.Context(), owner, cover.ID, "", 30)
	if err != nil || len(results) != 1 || results[0].ErrorCode == nil || *results[0].ErrorCode != "TARGET_CHANGED" {
		t.Fatal("cover silently overwrote another operation's version", results, err)
	}
	state, err := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album")
	if err != nil || state.Version != 2 || state.CoverOverrideID != nil || state.CoverMediaID == nil || *state.CoverMediaID != "cover-one" {
		t.Fatal("conflicting cover changed unrelated data", state, err)
	}
	for _, kind := range []string{"scan", "album_cover_batches", "batch_covers;DROP TABLE jobs", "", "BATCH_TAGS"} {
		if _, ok := tablesForAlbumBatch(kind); ok {
			t.Fatal("untrusted kind selected SQL identifiers", kind)
		}
	}
}
