package access

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func favoriteCounts(t *testing.T, s *Service) [5]int64 {
	t.Helper()
	var value [5]int64
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorites),(SELECT COUNT(*) FROM change_log),
 (SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events WHERE event_type='album_favorite_changed'),
 (SELECT COUNT(*) FROM mutation_receipts WHERE operation='album.favorite.replace')`).Scan(&value[0], &value[1], &value[2], &value[3], &value[4]); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAlbumFavoritesPrivateLifecycleAssignmentsReceiptsConcurrencyAndFreshAuthority(t *testing.T) {
	// Reuse the existing two-account personal-resource grant fixture, not new auth shortcuts.
	s, owner, viewer := ratingFixture(t)
	const id = "metadata-album"
	instant := time.Now().UTC().Truncate(time.Millisecond).Add(time.Hour)
	s.now = func() time.Time { return instant }
	if _, err := s.db.Exec(`INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES('favorite-owner',?,'Own',0),('favorite-viewer',?,'Own',0)`, owner.User.ID, viewer.User.ID); err != nil {
		t.Fatal(err)
	}
	initial, err := s.AlbumFavorite(t.Context(), owner, id)
	if err != nil || initial.AlbumID != id || initial.Favorited || initial.CollectionID != nil || initial.UpdatedAt != nil || initial.Version != 1 {
		t.Fatal("never-favorited state was invented/unversioned", initial, err)
	}
	album, err := s.Album(t.Context(), owner, id)
	if err != nil {
		t.Fatal(err)
	}
	counts := favoriteCounts(t, s)
	noop, err := s.SetAlbumFavorite(t.Context(), owner, id, 1, false, nil, store.NewID())
	after := favoriteCounts(t, s)
	after[4]--
	if err != nil || !reflect.DeepEqual(noop, initial) || after != counts {
		t.Fatal("initial clear materialized a row/event instead of only a receipt", noop, counts, after, err)
	}
	key := store.NewID()
	changed, err := s.SetAlbumFavorite(t.Context(), owner, id, 1, true, nil, key)
	if err != nil || !changed.Favorited || changed.CollectionID != nil || changed.Version != 2 || changed.UpdatedAt == nil || !changed.UpdatedAt.Equal(instant) {
		t.Fatal("first favorite lost its personal revision/server instant", changed, err)
	}
	counts = favoriteCounts(t, s)
	noop, err = s.SetAlbumFavorite(t.Context(), owner, id, 2, true, nil, store.NewID())
	after = favoriteCounts(t, s)
	after[4]--
	if err != nil || !reflect.DeepEqual(noop, changed) || after != counts {
		t.Fatal("identical membership changed business state/version/time/events", noop, counts, after, err)
	}
	other, err := s.AlbumFavorite(t.Context(), viewer, id)
	if err != nil || other.Favorited || other.Version != 1 {
		t.Fatal("one account's favorite leaked into another state", other, err)
	}
	if _, err := s.SetAlbumFavorite(t.Context(), viewer, id, 1, true, new("favorite-viewer"), store.NewID()); err != nil {
		t.Fatal(err)
	}
	moveKey := store.NewID()
	moved, err := s.SetAlbumFavorite(t.Context(), owner, id, 2, true, new("favorite-owner"), moveKey)
	if err != nil || moved.Version != 3 || moved.CollectionID == nil || *moved.CollectionID != "favorite-owner" {
		t.Fatal("owned assignment did not advance only its personal resource", moved, err)
	}
	for _, collection := range []string{"favorite-viewer", "missing", strings.Repeat("a", 64), strings.Repeat("字", 21) + "a", "literal'collection"} {
		counts := favoriteCounts(t, s)
		_, err := s.SetAlbumFavorite(t.Context(), owner, id, 3, true, &collection, store.NewID())
		requireError(t, err, sql.ErrNoRows)
		if favoriteCounts(t, s) != counts {
			t.Fatal("foreign/missing collection probe changed private state")
		}
	}
	for _, collection := range []string{"", " padded", "a/b", `a\b`, "bad\x00", "bad" + string(rune(0x200d)), strings.Repeat("a", 65), strings.Repeat("字", 22), string([]byte{0xff})} {
		_, err := s.SetAlbumFavorite(t.Context(), owner, id, 3, true, &collection, store.NewID())
		requireError(t, err, ErrAlbumFavorite)
	}
	_, err = s.SetAlbumFavorite(t.Context(), owner, id, 3, false, new("favorite-owner"), store.NewID())
	requireError(t, err, ErrAlbumFavorite)
	_, err = s.SetAlbumFavorite(t.Context(), owner, id, 3, true, nil, "invalid")
	requireError(t, err, store.ErrIdempotencyKey)
	cleared, err := s.SetAlbumFavorite(t.Context(), owner, id, 3, false, nil, store.NewID())
	if err != nil || cleared.Favorited || cleared.CollectionID != nil || cleared.Version != 4 || cleared.UpdatedAt == nil {
		t.Fatal("clear lost its retained personal revision", cleared, err)
	}
	var tombstone int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM favorites WHERE user_id=? AND album_id=? AND favorited=0 AND collection_id IS NULL AND version=4`, owner.User.ID, id).Scan(&tombstone); err != nil || tombstone != 1 {
		t.Fatal("clear erased its revision or kept an assignment", tombstone, err)
	}
	var scope, operation, payload string
	if err := s.db.QueryRow(`SELECT actor_scope,operation,payload_json FROM change_log WHERE kind='favorite' AND resource_id=? ORDER BY seq DESC LIMIT 1`, id).Scan(&scope, &operation, &payload); err != nil {
		t.Fatal(err)
	}
	var event AlbumFavorite
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &event) != nil || json.Unmarshal([]byte(payload), &fields) != nil || len(fields) != 5 || scope != owner.User.ScopeID || operation != "delete" || !reflect.DeepEqual(event, cleared) {
		t.Fatal("favorite tombstone was not complete and actor-private", scope, operation, payload)
	}
	replay, err := s.SetAlbumFavorite(t.Context(), owner, id, 1, true, nil, key)
	if err != nil || !reflect.DeepEqual(replay, changed) {
		t.Fatal("historical default favorite receipt was recomputed", replay, changed, err)
	}
	if _, err := s.db.Exec(`DELETE FROM favorite_collections WHERE id='favorite-owner'`); err != nil {
		t.Fatal(err)
	}
	replay, err = s.SetAlbumFavorite(t.Context(), owner, id, 2, true, new("favorite-owner"), moveKey)
	if err != nil || !reflect.DeepEqual(replay, moved) {
		t.Fatal("historical receipt was retargeted after its cleared collection was removed", replay, moved, err)
	}
	current, err := s.AlbumFavorite(t.Context(), owner, id)
	if err != nil || !reflect.DeepEqual(current, cleared) {
		t.Fatal("historical replay restored an obsolete membership/assignment", current, cleared, err)
	}
	for _, version := range []int64{1, 2, 3} {
		_, err := s.SetAlbumFavorite(t.Context(), owner, id, version, true, nil, store.NewID())
		requireError(t, err, auth.ErrConflict)
	}
	_, err = s.SetAlbumFavorite(t.Context(), owner, id, 1, false, nil, key)
	requireError(t, err, store.ErrIdempotencyReuse)
	_, err = s.SetAlbumFavorite(t.Context(), owner, id, 2, true, nil, key)
	requireError(t, err, store.ErrIdempotencyReuse)
	originalInstant := instant
	instant = instant.Add(-time.Minute)
	recreated, err := s.SetAlbumFavorite(t.Context(), owner, id, 4, true, nil, store.NewID())
	if err != nil || recreated.Version != 5 || !recreated.Favorited || recreated.UpdatedAt == nil || !recreated.UpdatedAt.Equal(originalInstant) {
		t.Fatal("re-add reused a revision or moved the private server clock backward", recreated, err)
	}
	public, err := s.Album(t.Context(), owner, id)
	if err != nil || !reflect.DeepEqual(public, album) {
		t.Fatal("private favorites changed shared album metadata/average/version", public, album, err)
	}
	freshOwner, err := s.auth.Authenticate(t.Context(), owner.Token)
	if err != nil || freshOwner.User.ScopeID != owner.User.ScopeID {
		t.Fatal("private favorite changed authorization scope", freshOwner.User.ScopeID, err)
	}
	instant = originalInstant.Add(25 * time.Hour)
	_, err = s.SetAlbumFavorite(t.Context(), owner, id, 1, true, nil, key)
	requireError(t, err, auth.ErrConflict)
	if value, err := s.SetAlbumFavorite(t.Context(), owner, id, 5, true, nil, key); err != nil || !reflect.DeepEqual(value, recreated) {
		t.Fatal("expired key could not be reused with current no-op state", value, err)
	}
	for _, scenario := range []struct {
		id     string
		replay bool
	}{{"metadata-empty", false}, {"metadata-other", true}} {
		counts := favoriteCounts(t, s)
		requestKey := store.NewID()
		start := make(chan struct{})
		results := make(chan error, 8)
		var workers sync.WaitGroup
		for range 8 {
			workers.Go(func() {
				<-start
				key := requestKey
				if !scenario.replay {
					key = store.NewID()
				}
				value, err := s.SetAlbumFavorite(t.Context(), owner, scenario.id, 1, true, nil, key)
				if err == nil && (value.Version != 2 || !value.Favorited) {
					err = errors.New("concurrent favorite returned an incorrect state")
				}
				results <- err
			})
		}
		close(start)
		workers.Wait()
		close(results)
		applied, conflicts := 0, 0
		for err := range results {
			switch {
			case err == nil:
				applied++
			case errors.Is(err, auth.ErrConflict):
				conflicts++
			default:
				t.Fatal("concurrent favorite failed outside the version contract", err)
			}
		}
		if scenario.replay && (applied != 8 || conflicts != 0) || !scenario.replay && (applied != 1 || conflicts != 7) {
			t.Fatal("concurrent favorite effects/replays were duplicated", scenario, applied, conflicts)
		}
		want := counts
		for i := range want {
			want[i]++
		}
		if after := favoriteCounts(t, s); after != want {
			t.Fatal("concurrent favorite duplicated a business effect", counts, after, want)
		}
	}
	forged := viewer
	forged.User = owner.User
	if value, err := s.SetAlbumFavorite(t.Context(), forged, id, 2, false, nil, store.NewID()); err != nil || value.Favorited || value.Version != 3 {
		t.Fatal("forged user DTO replaced actual session authority", value, err)
	}
	if own, err := s.AlbumFavorite(t.Context(), owner, id); err != nil || !reflect.DeepEqual(own, recreated) {
		t.Fatal("forged actor changed another account's membership", own, err)
	}
	for _, query := range []string{
		`UPDATE libraries SET is_active=0 WHERE id='metadata-library'`,
		`UPDATE sources SET is_active=0 WHERE id='metadata-source'`,
		`UPDATE sources SET library_id='metadata-destination' WHERE id='metadata-source'`,
	} {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO libraries(id,name,created_at) VALUES('metadata-destination','Other',0)`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(query); err != nil {
			t.Fatal(err)
		}
		counts := favoriteCounts(t, s)
		for _, actor := range []auth.Session{viewer, owner} {
			_, err := s.AlbumFavorite(t.Context(), actor, id)
			requireError(t, err, sql.ErrNoRows)
			_, err = s.SetAlbumFavorite(t.Context(), actor, id, 5, true, nil, key)
			requireError(t, err, sql.ErrNoRows)
		}
		if favoriteCounts(t, s) != counts {
			t.Fatal("hidden favorite resource replayed/changed a receipt")
		}
		if _, err := s.db.Exec(`UPDATE libraries SET is_active=1 WHERE id='metadata-library'; UPDATE sources SET is_active=1,library_id='metadata-library' WHERE id='metadata-source'`); err != nil {
			t.Fatal(err)
		}
	}
	membership(t, s, owner, viewer)
	_, err = s.AlbumFavorite(t.Context(), viewer, id)
	requireError(t, err, sql.ErrNoRows)
	_, err = s.SetAlbumFavorite(t.Context(), viewer, id, 3, false, nil, store.NewID())
	requireError(t, err, sql.ErrNoRows)
	_, err = s.AlbumFavorite(t.Context(), owner, "missing")
	requireError(t, err, sql.ErrNoRows)
	if _, err := s.db.Exec(`UPDATE users SET active=0 WHERE id=?`, viewer.User.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.SetAlbumFavorite(t.Context(), viewer, id, 3, false, nil, store.NewID())
	requireError(t, err, auth.ErrUnauthenticated)
	_, err = s.AlbumFavorite(t.Context(), auth.Session{}, id)
	requireError(t, err, auth.ErrUnauthenticated)
	var missing int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE o.id IS NULL`).Scan(&missing); err != nil || missing != 0 {
		t.Fatal("favorite private change lacks its atomic outbox", missing, err)
	}
}

func TestAlbumFavoritesLateFailuresAndOverflowLeavePersonalAndSharedStateAtomic(t *testing.T) {
	s, owner := albumMetadataFixture(t)
	const id = "metadata-album"
	before, err := s.SetAlbumFavorite(t.Context(), owner, id, 1, true, nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	album, err := s.Album(t.Context(), owner, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []string{
		`CREATE TRIGGER fail_favorite BEFORE UPDATE ON favorites BEGIN SELECT RAISE(ABORT,'injected membership failure'); END`,
		`CREATE TRIGGER fail_favorite BEFORE INSERT ON change_log WHEN NEW.kind='favorite' BEGIN SELECT RAISE(ABORT,'injected private journal failure'); END`,
		`CREATE TRIGGER fail_favorite BEFORE INSERT ON outbox_events WHEN EXISTS(SELECT 1 FROM change_log c WHERE c.seq=NEW.change_seq AND c.kind='favorite') BEGIN SELECT RAISE(ABORT,'injected private outbox failure'); END`,
		`CREATE TRIGGER fail_favorite BEFORE INSERT ON audit_events WHEN NEW.event_type='album_favorite_changed' BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`,
		`CREATE TRIGGER fail_favorite BEFORE INSERT ON mutation_receipts WHEN NEW.operation='album.favorite.replace' BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`,
	} {
		counts := favoriteCounts(t, s)
		if _, err := s.db.Exec(trigger); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetAlbumFavorite(t.Context(), owner, id, 2, false, nil, store.NewID()); err == nil {
			t.Fatal("favorite clear committed without all late effects", trigger)
		}
		value, err := s.AlbumFavorite(t.Context(), owner, id)
		if err != nil || !reflect.DeepEqual(value, before) || favoriteCounts(t, s) != counts {
			t.Fatal("late favorite failure retained a partial membership/version/event", value, before, err)
		}
		if _, err := s.db.Exec(`DROP TRIGGER fail_favorite`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE favorites SET version=? WHERE user_id=? AND album_id=?`, int64(math.MaxInt64), owner.User.ID, id); err != nil {
		t.Fatal(err)
	}
	counts := favoriteCounts(t, s)
	_, err = s.SetAlbumFavorite(t.Context(), owner, id, math.MaxInt64, false, nil, store.NewID())
	requireError(t, err, auth.ErrConflict)
	if favoriteCounts(t, s) != counts {
		t.Fatal("favorite version overflow partially applied")
	}
	if value, err := s.SetAlbumFavorite(t.Context(), owner, id, math.MaxInt64, true, nil, store.NewID()); err != nil || value.Version != math.MaxInt64 {
		t.Fatal("overflow guard rejected a genuine no-op", value, err)
	}
	public, err := s.Album(t.Context(), owner, id)
	if err != nil || !reflect.DeepEqual(public, album) {
		t.Fatal("favorite errors/no-op changed shared metadata", public, album, err)
	}
	if _, err := s.db.Exec(`UPDATE albums SET version=? WHERE id='metadata-empty'`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if value, err := s.SetAlbumFavorite(t.Context(), owner, "metadata-empty", 1, true, nil, store.NewID()); err != nil || value.Version != 2 {
		t.Fatal("a shared version incorrectly gated an independent personal favorite", value, err)
	}
	if _, err := s.db.Exec(`UPDATE favorites SET updated_at=? WHERE user_id=? AND album_id=?`, int64(math.MaxInt64), owner.User.ID, id); err != nil {
		t.Fatal(err)
	}
	counts = favoriteCounts(t, s)
	if _, err := s.AlbumFavorite(t.Context(), owner, id); err == nil {
		t.Fatal("invalid stored favorite instant was serialized")
	}
	if _, err := s.SetAlbumFavorite(t.Context(), owner, id, math.MaxInt64, true, nil, store.NewID()); err == nil || favoriteCounts(t, s) != counts {
		t.Fatal("invalid stored instant committed a success/receipt", err)
	}
}
