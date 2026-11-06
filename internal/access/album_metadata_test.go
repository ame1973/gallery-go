package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func albumMetadataFixture(t *testing.T) (*Service, auth.Session) {
	t.Helper()
	s, owner := fixture(t)
	if _, err := s.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('metadata-library','Metadata',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('metadata-source','metadata-library','one','/private/never-open-album-source','{}','unavailable',0),('metadata-other-source','metadata-library','other','/private/never-open-other-source','{}','healthy',0);
 INSERT INTO albums(id,library_id,source_id,source_name,cover_media_id,access_restricted,created_at,updated_at) VALUES
 ('metadata-album','metadata-library','metadata-source','Indexed folder','cover-one',1,0,0),
 ('metadata-empty','metadata-library','metadata-source','Empty',NULL,0,0,0),
 ('metadata-other','metadata-library','metadata-source','Other',NULL,0,0,0);
 INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES
 ('cover-one','metadata-album','image','one.jpg',0,0),('cover-two','metadata-album','image','two.jpg',0,0),
 ('cover-foreign','metadata-other','image','foreign.jpg',0,0),('cover-archived','metadata-album','image','archived.jpg',0,0),('cover-not-media','metadata-album','image','invalid.jpg',0,0);
 UPDATE media SET availability='archived' WHERE id='cover-archived';
 UPDATE media SET metadata_json='{"status":"not_media"}' WHERE id='cover-not-media';
 INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,updated_at) VALUES('wrong-cover-entry','metadata-other-source','never-open.jpg',1,0,0);
 INSERT INTO media(id,album_id,source_entry_id,kind,file_name,created_at,updated_at) VALUES('cover-wrong-source','metadata-album','wrong-cover-entry','image','wrong.jpg',0,0)`); err != nil {
		t.Fatal(err)
	}
	return s, owner
}

func albumMetadataCounts(t *testing.T, s *Service) [4]int64 {
	t.Helper()
	var counts [4]int64
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log WHERE kind='album' AND resource_id='metadata-album'),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events WHERE event_type='album_metadata_updated'),(SELECT COUNT(*) FROM mutation_receipts WHERE operation='album.metadata.update')`).Scan(&counts[0], &counts[1], &counts[2], &counts[3]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestAlbumMetadataOverridesAreVersionedReplayableAndPreserveSourceAndPolicy(t *testing.T) {
	s, owner := albumMetadataFixture(t)
	before, err := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album")
	if err != nil || before.Version != 1 || before.SourceName != "Indexed folder" || before.DisplayName != nil || before.CoverOverrideID != nil || before.CoverMediaID == nil || *before.CoverMediaID != "cover-one" {
		t.Fatal("incorrect initial metadata projection", before, err)
	}
	counts := albumMetadataCounts(t, s)
	key := store.NewID()
	patch := AlbumMetadataPatch{DisplayName: new("  Display override  "), CoverMediaID: new("cover-two")}
	changed, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, patch, key)
	if err != nil || changed.Version != 2 || changed.SourceName != before.SourceName || changed.DisplayName == nil || *changed.DisplayName != "Display override" || changed.CoverOverrideID == nil || *changed.CoverOverrideID != "cover-two" || changed.CoverMediaID == nil || *changed.CoverMediaID != "cover-two" {
		t.Fatal("metadata did not atomically apply the normalized overrides", changed, err)
	}
	afterCounts := albumMetadataCounts(t, s)
	for index := range counts {
		if afterCounts[index] != counts[index]+1 {
			t.Fatal("resource/event/outbox/audit/receipt not co-committed", counts, afterCounts)
		}
	}
	replay, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, patch, key)
	if err != nil || !reflect.DeepEqual(replay, changed) || albumMetadataCounts(t, s) != afterCounts {
		t.Fatal("receipt replay changed result or duplicated mutation events", replay, changed, err)
	}
	noop, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 2, patch, store.NewID())
	if err != nil || !reflect.DeepEqual(noop, changed) {
		t.Fatal("same overrides did not remain a version/event no-op", noop, err)
	}
	noopCounts := albumMetadataCounts(t, s)
	if noopCounts[0] != afterCounts[0] || noopCounts[1] != afterCounts[1] || noopCounts[2] != afterCounts[2] || noopCounts[3] != afterCounts[3]+1 {
		t.Fatal("no-op must save only its receipt, never business events", afterCounts, noopCounts)
	}
	cleared, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 2, AlbumMetadataPatch{DisplayName: new(""), CoverMediaID: new("")}, store.NewID())
	if err != nil || cleared.Version != 3 || cleared.DisplayName != nil || cleared.CoverOverrideID != nil || cleared.CoverMediaID == nil || *cleared.CoverMediaID != "cover-two" {
		t.Fatal("clearing overrides lost sticky automatic cover/source defaults", cleared, err)
	}
	var restricted bool
	var source, cover, label string
	if err := s.db.QueryRow(`SELECT access_restricted,source_id,cover_media_id,source_name FROM albums WHERE id='metadata-album'`).Scan(&restricted, &source, &cover, &label); err != nil || !restricted || source != "metadata-source" || cover != "cover-two" || label != "Indexed folder" {
		t.Fatal("metadata patch rewrote source identity or restriction latch", restricted, source, cover, label, err)
	}
	staleReplay, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, patch, key)
	if err != nil || !reflect.DeepEqual(staleReplay, changed) {
		t.Fatal("retry after a later mutation lost its historical result", staleReplay, changed, err)
	}
	if _, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, patch, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("fresh stale command ignored resource version", err)
	}
	if _, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, AlbumMetadataPatch{DisplayName: new("different")}, key); !errors.Is(err, store.ErrIdempotencyReuse) {
		t.Fatal("same actor/key accepted a different mutation", err)
	}
	var payload string
	if err := s.db.QueryRow(`SELECT payload_json FROM change_log WHERE kind='album' AND resource_id='metadata-album' ORDER BY seq LIMIT 1`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &event) != nil {
		t.Fatal("invalid album event", payload)
	}
	for _, field := range []string{"id", "library_id", "source_id", "person_id", "source_name", "display_name", "cover_media_id", "restricted", "quarantined_at", "tag_ids", "version"} {
		if _, ok := event[field]; !ok {
			t.Fatal("metadata mutation omitted part of the canonical album event", field, payload)
		}
	}
}

func TestAlbumMetadataRejectsForeignArchivedNonMediaAndMismatchedSourceCovers(t *testing.T) {
	s, owner := albumMetadataFixture(t)
	before, _ := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album")
	counts := albumMetadataCounts(t, s)
	for _, cover := range []string{"cover-foreign", "cover-archived", "cover-not-media", "cover-wrong-source", "missing", " ", strings.Repeat("x", 65)} {
		if _, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, AlbumMetadataPatch{CoverMediaID: &cover}, store.NewID()); !errors.Is(err, ErrAlbumCover) {
			t.Fatal("invalid/foreign current cover was accepted", cover, err)
		}
	}
	if _, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, AlbumMetadataPatch{}, store.NewID()); !errors.Is(err, ErrAlbumMetadata) {
		t.Fatal("empty patch accepted", err)
	}
	if _, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, AlbumMetadataPatch{DisplayName: new("  ")}, store.NewID()); !errors.Is(err, ErrName) {
		t.Fatal("ambiguous blank label cleared an override", err)
	}
	after, err := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album")
	if err != nil || !reflect.DeepEqual(after, before) || albumMetadataCounts(t, s) != counts {
		t.Fatal("rejected cover/name changed state or side effects", before, after, err)
	}
	empty, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-empty", 1, AlbumMetadataPatch{CoverMediaID: new("")}, store.NewID())
	if err != nil || empty.Version != 1 || empty.CoverMediaID != nil || empty.CoverOverrideID != nil {
		t.Fatal("empty album did not preserve nullable automatic cover", empty, err)
	}
	if _, err := s.db.Exec(`UPDATE albums SET cover_media_id='cover-foreign',cover_override_id='cover-two' WHERE id='metadata-album'; UPDATE media SET album_id='metadata-other' WHERE id='cover-two'`); err != nil {
		t.Fatal(err)
	}
	unavailable, err := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album")
	if err != nil || unavailable.CoverMediaID != nil || unavailable.CoverOverrideID == nil || *unavailable.CoverOverrideID != "cover-two" {
		t.Fatal("historical desired cover became foreign content authority", unavailable, err)
	}
	if _, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, AlbumMetadataPatch{CoverMediaID: new("cover-two")}, store.NewID()); !errors.Is(err, ErrAlbumCover) {
		t.Fatal("new intent blessed a moved historical cover preference", err)
	}
}

func TestAlbumMetadataConcurrentReceiptsAndVersionsAllowOneBusinessWrite(t *testing.T) {
	s, owner := albumMetadataFixture(t)
	counts := albumMetadataCounts(t, s)
	key := store.NewID()
	patch := AlbumMetadataPatch{DisplayName: new("Concurrent"), CoverMediaID: new("cover-two")}
	type outcome struct {
		value AlbumMetadata
		err   error
	}
	results := make(chan outcome, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			value, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, patch, key)
			results <- outcome{value, err}
		})
	}
	group.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.value.Version != 2 || result.value.DisplayName == nil || *result.value.DisplayName != "Concurrent" {
			t.Fatal("same command raced past its actor-bound receipt", result.value, result.err)
		}
	}
	after := albumMetadataCounts(t, s)
	for index := range counts {
		if after[index] != counts[index]+1 {
			t.Fatal("concurrent receipt created duplicate side effects", counts, after)
		}
	}
	conflicts := make(chan error, 2)
	for _, title := range []string{"First", "Second"} {
		group.Go(func() {
			_, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 2, AlbumMetadataPatch{DisplayName: &title}, store.NewID())
			conflicts <- err
		})
	}
	group.Wait()
	close(conflicts)
	succeeded, stale := 0, 0
	for err := range conflicts {
		if err == nil {
			succeeded++
		} else if errors.Is(err, auth.ErrConflict) {
			stale++
		} else {
			t.Fatal("unexpected concurrent metadata result", err)
		}
	}
	if succeeded != 1 || stale != 1 {
		t.Fatal("distinct commands both applied one resource version", succeeded, stale)
	}
}

func TestAlbumMetadataLateJournalOrReceiptFailureRollsBackAllState(t *testing.T) {
	for _, table := range []string{"change_log", "outbox_events", "audit_events", "mutation_receipts"} {
		t.Run(table, func(t *testing.T) {
			s, owner := albumMetadataFixture(t)
			before, _ := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album")
			counts := albumMetadataCounts(t, s)
			if _, err := s.db.Exec(`CREATE TRIGGER reject_metadata BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected private metadata journal failure'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, AlbumMetadataPatch{DisplayName: new("Should roll back"), CoverMediaID: new("cover-two")}, store.NewID()); err == nil {
				t.Fatal("late business journal failure was ignored")
			}
			after, err := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album")
			if err != nil || !reflect.DeepEqual(before, after) || albumMetadataCounts(t, s) != counts {
				t.Fatal("partial metadata/version/event/audit/receipt survived rollback", before, after, err)
			}
		})
	}
}

func TestAlbumMetadataCommandsReauthorizeBeforeHistoryAndHideInactiveResources(t *testing.T) {
	s, owner := albumMetadataFixture(t)
	viewer := reader(t, s, "metadata-reader")
	forged := viewer
	forged.User.Role = "admin"
	for _, session := range []auth.Session{viewer, forged} {
		if _, err := s.AdminAlbumMetadata(t.Context(), session, "metadata-album"); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller DTO role authorized admin metadata", err)
		}
		if _, err := s.UpdateAlbumMetadata(t.Context(), session, "metadata-album", 1, AlbumMetadataPatch{DisplayName: new("Denied")}, store.NewID()); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller DTO role authorized an override", err)
		}
	}
	key := store.NewID()
	patch := AlbumMetadataPatch{DisplayName: new("Receipt")}
	if _, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, patch, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET role='user',version=version+1 WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAlbumMetadata(t.Context(), owner, "metadata-album", 1, patch, key); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("historical receipt bypassed fresh administrator authorization", err)
	}
	if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE id=?; UPDATE sources SET is_active=0 WHERE id='metadata-source'`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminAlbumMetadata(t.Context(), owner, "metadata-album"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("inactive source metadata became available", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.UpdateAlbumMetadata(ctx, owner, "metadata-album", 2, patch, store.NewID()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled metadata mutation proceeded", err)
	}
}
