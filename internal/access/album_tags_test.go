package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func albumTagsFixture(t *testing.T) (*Service, auth.Session) {
	t.Helper()
	s, owner := albumMetadataFixture(t)
	if _, err := s.db.Exec(`INSERT INTO tags(id,name) VALUES('tag-one','One'),('tag-two','Two'),('tag-unused','Private unused')`); err != nil {
		t.Fatal(err)
	}
	return s, owner
}

func albumTagCounts(t *testing.T, s *Service) [4]int64 {
	t.Helper()
	var counts [4]int64
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log WHERE kind='album' AND resource_id='metadata-album'),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events WHERE event_type='album_tags_changed'),(SELECT COUNT(*) FROM mutation_receipts WHERE operation='album.tags.replace')`).Scan(&counts[0], &counts[1], &counts[2], &counts[3]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func assertAlbumEventTags(t *testing.T, s *Service, id string, ids []string) {
	t.Helper()
	var data string
	if err := s.db.QueryRow(`SELECT payload_json FROM change_log WHERE kind='album' AND resource_id=? ORDER BY seq DESC LIMIT 1`, id).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &payload) != nil || len(payload) != 13 {
		t.Fatal("album event is not the complete canonical projection", data)
	}
	for _, field := range []string{"id", "library_id", "source_id", "person_id", "source_name", "display_name", "cover_media_id", "restricted", "quarantined_at", "tag_ids", "rating_avg", "indexed_at", "version"} {
		if _, ok := payload[field]; !ok {
			t.Fatal("album event lost an existing canonical field", field, data)
		}
	}
	var indexed time.Time
	var millis int64
	if err := s.db.QueryRow(`SELECT created_at FROM albums WHERE id=?`, id).Scan(&millis); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload["indexed_at"], &indexed); err != nil || !indexed.Equal(time.UnixMilli(millis).UTC()) {
		t.Fatal("album event changed its first-indexed public ordering key", indexed, millis, err)
	}
	var average, expected *float64
	if err := s.db.QueryRow(`SELECT AVG(rating) FROM ratings WHERE album_id=?`, id).Scan(&expected); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload["rating_avg"], &average); err != nil || !reflect.DeepEqual(average, expected) {
		t.Fatal("album event lost its current shared rating aggregate", average, expected, err)
	}
	var actual []string
	if err := json.Unmarshal(payload["tag_ids"], &actual); err != nil || actual == nil || !slices.Equal(actual, ids) {
		t.Fatal("album event lost or nulled its canonical tag set", actual, ids, err)
	}
}

func TestAlbumTagsCommandsAreVersionedCanonicalReplayableAndClearOnlyEdges(t *testing.T) {
	s, owner := albumTagsFixture(t)
	before, err := s.AdminAlbumTags(t.Context(), owner, "metadata-album")
	if err != nil || before.AlbumID != "metadata-album" || before.Version != 1 || before.TagIDs == nil || len(before.TagIDs) != 0 {
		t.Fatal("empty association metadata was not explicit and versioned", before, err)
	}
	counts := albumTagCounts(t, s)
	key := store.NewID()
	ids := []string{"tag-two", "tag-one"}
	changed, err := s.SetAlbumTags(t.Context(), owner, before.AlbumID, 1, ids, key)
	if err != nil || changed.Version != 2 || !slices.Equal(changed.TagIDs, []string{"tag-one", "tag-two"}) || !slices.Equal(ids, []string{"tag-two", "tag-one"}) {
		t.Fatal("tag replacement lost version/canonical ordering or mutated caller input", changed, ids, err)
	}
	after := albumTagCounts(t, s)
	for index := range counts {
		if after[index] != counts[index]+1 {
			t.Fatal("association/version/change/outbox/audit/receipt did not co-commit", counts, after)
		}
	}
	assertAlbumEventTags(t, s, before.AlbumID, changed.TagIDs)
	replay, err := s.SetAlbumTags(t.Context(), owner, before.AlbumID, 1, []string{"tag-one", "tag-two"}, key)
	if err != nil || !reflect.DeepEqual(replay, changed) || albumTagCounts(t, s) != after {
		t.Fatal("unordered equivalent set did not replay the canonical actor receipt", replay, changed, err)
	}
	noop, err := s.SetAlbumTags(t.Context(), owner, before.AlbumID, 2, ids, store.NewID())
	if err != nil || !reflect.DeepEqual(noop, changed) {
		t.Fatal("same tag set was not a version no-op", noop, changed, err)
	}
	noopCounts := albumTagCounts(t, s)
	if noopCounts[0] != after[0] || noopCounts[1] != after[1] || noopCounts[2] != after[2] || noopCounts[3] != after[3]+1 {
		t.Fatal("tag no-op wrote business events or omitted a receipt", after, noopCounts)
	}
	if _, err := s.SetAlbumTags(t.Context(), owner, before.AlbumID, 1, ids, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("new stale replacement ignored the shared album version", err)
	}
	if _, err := s.SetAlbumTags(t.Context(), owner, before.AlbumID, 2, ids, key); !errors.Is(err, store.ErrIdempotencyReuse) {
		t.Fatal("same key accepted another album precondition", err)
	}
	if err := s.DeleteTag(t.Context(), owner, "tag-one", 1, store.NewID()); !errors.Is(err, ErrTagInUse) {
		t.Fatal("assigned tag could be removed implicitly", err)
	}
	cleared, err := s.SetAlbumTags(t.Context(), owner, before.AlbumID, 2, []string{}, store.NewID())
	if err != nil || cleared.Version != 3 || cleared.TagIDs == nil || len(cleared.TagIDs) != 0 {
		t.Fatal("explicit empty set did not clear only the links", cleared, err)
	}
	assertAlbumEventTags(t, s, before.AlbumID, []string{})
	var tags int
	var restricted bool
	var source, label, cover string
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM tags),access_restricted,source_id,source_name,cover_media_id FROM albums WHERE id='metadata-album'`).Scan(&tags, &restricted, &source, &label, &cover); err != nil || tags != 3 || !restricted || source != "metadata-source" || label != "Indexed folder" || cover != "cover-one" {
		t.Fatal("tag command changed catalog/identity/cover or cleared a restriction", tags, restricted, source, label, cover, err)
	}
	replay, err = s.SetAlbumTags(t.Context(), owner, before.AlbumID, 1, ids, key)
	if err != nil || !reflect.DeepEqual(replay, changed) {
		t.Fatal("historical tag receipt was retargeted by a later clear", replay, changed, err)
	}
}

func TestAlbumTagsRejectInvalidUnknownSetsRolesAndInactiveResources(t *testing.T) {
	s, owner := albumTagsFixture(t)
	counts := albumTagCounts(t, s)
	for _, ids := range [][]string{nil, {""}, {"tag-one", "tag-one"}, {strings.Repeat("x", 65)}, {"bad\xff"}, {"bad\x00id"}, make([]string, 101)} {
		if _, err := s.SetAlbumTags(t.Context(), owner, "metadata-album", 1, ids, store.NewID()); !errors.Is(err, ErrTagSet) {
			t.Fatal("invalid or ambiguous tag replacement was accepted", ids, err)
		}
	}
	if _, err := s.SetAlbumTags(t.Context(), owner, "metadata-album", 1, []string{"tag-one", "missing"}, store.NewID()); !errors.Is(err, ErrUnknownTag) {
		t.Fatal("unknown tag set was partially accepted", err)
	}
	if albumTagCounts(t, s) != counts {
		t.Fatal("invalid tag replacement produced side effects")
	}
	reader := reader(t, s, "album-tag-reader")
	forged := reader
	forged.User.Role = "admin"
	for _, viewer := range []auth.Session{reader, forged} {
		if _, err := s.AdminAlbumTags(t.Context(), viewer, "metadata-album"); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller role exposed admin association state", err)
		}
		if _, err := s.SetAlbumTags(t.Context(), viewer, "metadata-album", 1, []string{}, store.NewID()); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller role edited album tag associations", err)
		}
	}
	key := store.NewID()
	if _, err := s.SetAlbumTags(t.Context(), owner, "metadata-album", 1, []string{"tag-one"}, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET role='user',version=version+1 WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAlbumTags(t.Context(), owner, "metadata-album", 1, []string{"tag-one"}, key); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("historical association receipt bypassed fresh administrator checks", err)
	}
	if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE albums SET version=? WHERE id='metadata-album'`, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAlbumTags(t.Context(), owner, "metadata-album", math.MaxInt64, []string{}, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("album tag mutation allowed resource version overflow", err)
	}
	if _, err := s.db.Exec(`UPDATE sources SET is_active=0 WHERE id='metadata-source'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminAlbumTags(t.Context(), owner, "metadata-album"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("inactive source exposed association state", err)
	}
	if _, err := s.SetAlbumTags(t.Context(), owner, "metadata-album", math.MaxInt64, []string{}, store.NewID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("inactive source accepted association mutation", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.SetAlbumTags(ctx, owner, "metadata-album", 2, []string{}, store.NewID()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled tag mutation proceeded", err)
	}
}

func TestAlbumTagsPublicReadsShareVisibilityAndMetadataEditsRetainCanonicalEdges(t *testing.T) {
	f := indexedPeopleFixture(t)
	if _, err := f.svc.db.Exec(`INSERT INTO tags(id,name) VALUES('tag-one','Visible'),('tag-two','Private'),('tag-unused','Unused private')`); err != nil {
		t.Fatal(err)
	}
	open, err := f.svc.SetAlbumTags(t.Context(), f.owner, "person-open", 1, []string{"tag-one"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetAlbumTags(t.Context(), f.owner, "person-private", 2, []string{"tag-two"}, store.NewID()); err != nil {
		t.Fatal(err)
	}
	visible, err := f.svc.AlbumTags(t.Context(), f.base, "person-open", "", 30)
	if err != nil || len(visible) != 1 || visible[0].ID != "tag-one" || visible[0].Name != "Visible" {
		t.Fatal("public tag page leaked unrelated/private/unused labels", visible, err)
	}
	if _, err := f.svc.AlbumTags(t.Context(), f.base, "person-private", "", 30); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("hidden album exposed private tag names", err)
	}
	if _, err := f.svc.RenameTag(t.Context(), f.owner, "tag-one", 1, "Changed tag label", store.NewID()); err != nil {
		t.Fatal(err)
	}
	unchanged, err := f.svc.AdminAlbumTags(t.Context(), f.owner, "person-open")
	if err != nil || !reflect.DeepEqual(unchanged, open) {
		t.Fatal("separate tag-name mutation changed association IDs/album version", unchanged, open, err)
	}
	visible, err = f.svc.AlbumTags(t.Context(), f.base, "person-open", "", 30)
	if err != nil || visible[0].Name != "Changed tag label" || visible[0].Version != 2 {
		t.Fatal("authorized tag labels did not follow the tag's own metadata version", visible, err)
	}
	metadata, err := f.svc.UpdateAlbumMetadata(t.Context(), f.owner, "person-open", open.Version, AlbumMetadataPatch{DisplayName: new("Album label")}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	assertAlbumEventTags(t, f.svc, "person-open", []string{"tag-one"})
	if _, err := f.svc.SetAlbumPolicy(t.Context(), f.owner, "person-open", metadata.Version, true, []string{f.privateGroup.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteGroup(t.Context(), f.owner, f.privateGroup.ID, f.privateGroup.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AlbumTags(t.Context(), f.base, "person-open", "", 30); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("group deletion widened private tag visibility", err)
	}
	preserved, err := f.svc.AdminAlbumTags(t.Context(), f.owner, "person-open")
	if err != nil || !slices.Equal(preserved.TagIDs, []string{"tag-one"}) {
		t.Fatal("policy events/group deletion lost independent tag associations", preserved, err)
	}
}

func TestAlbumTagsConcurrentReceiptsAndLateFailuresRemainAtomic(t *testing.T) {
	s, owner := albumTagsFixture(t)
	key := store.NewID()
	counts := albumTagCounts(t, s)
	type outcome struct {
		value AlbumTagSet
		err   error
	}
	results := make(chan outcome, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			value, err := s.SetAlbumTags(t.Context(), owner, "metadata-album", 1, []string{"tag-one"}, key)
			results <- outcome{value, err}
		})
	}
	group.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.value.Version != 2 || !slices.Equal(result.value.TagIDs, []string{"tag-one"}) {
			t.Fatal("same-key concurrent tag command lost its result", result, result.err)
		}
	}
	after := albumTagCounts(t, s)
	for index := range counts {
		if after[index] != counts[index]+1 {
			t.Fatal("same-key tag commands duplicated business side effects", counts, after)
		}
	}
	for _, table := range []string{"change_log", "outbox_events", "audit_events", "mutation_receipts"} {
		t.Run(table, func(t *testing.T) {
			before, err := s.AdminAlbumTags(t.Context(), owner, "metadata-album")
			if err != nil {
				t.Fatal(err)
			}
			counts := albumTagCounts(t, s)
			if _, err := s.db.Exec(`CREATE TRIGGER reject_album_tag_journal BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'private association journal failure'); END`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, err := s.db.Exec(`DROP TRIGGER reject_album_tag_journal`)
				if err != nil {
					t.Error(err)
				}
			})
			if _, err := s.SetAlbumTags(t.Context(), owner, "metadata-album", 2, []string{"tag-two"}, store.NewID()); err == nil {
				t.Fatal("late association journal failure was ignored")
			}
			current, err := s.AdminAlbumTags(t.Context(), owner, "metadata-album")
			if err != nil || !reflect.DeepEqual(current, before) || albumTagCounts(t, s) != counts {
				t.Fatal("partial tag replacement/version/events survived rollback", current, before, err)
			}
		})
	}
	conflicts := make(chan error, 2)
	for _, ids := range [][]string{{}, {"tag-two"}} {
		group.Go(func() {
			_, err := s.SetAlbumTags(t.Context(), owner, "metadata-album", 2, ids, store.NewID())
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
			t.Fatal("unexpected competing tag replacement result", err)
		}
	}
	if succeeded != 1 || stale != 1 {
		t.Fatal("competing tag replacements both applied one album version", succeeded, stale)
	}
}
