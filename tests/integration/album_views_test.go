package integration

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func seedAlbumViews(t *testing.T, f collectionFixture) {
	t.Helper()
	if _, err := f.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('view-library','Views',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('view-source','view-library','Views','/never-open-album-views','{}','unavailable',0);
 INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES('view-a','view-library','view-source','A',0,0),('view-b','view-library','view-source','B',0,0);
 INSERT INTO permission_groups(id,name,created_at) VALUES('view-group','View readers',0);
 INSERT INTO library_access(library_id,group_id) VALUES('view-library','view-group')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO user_groups(user_id,group_id) VALUES(?,'view-group')`, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
}

func albumViewState(t *testing.T, f collectionFixture) [8]int64 {
	t.Helper()
	var values [8]int64
	if err := f.db.QueryRow(`SELECT
 (SELECT COALESCE(SUM(view_count),0) FROM album_views WHERE album_id='view-a'),
 (SELECT view_count FROM albums WHERE id='view-a'),(SELECT version FROM albums WHERE id='view-a'),
 (SELECT COUNT(*) FROM change_log WHERE kind='album' AND resource_id='view-a'),
 (SELECT COUNT(*) FROM change_log WHERE kind='album_view' AND resource_id='view-a'),
 (SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.resource_id='view-a' AND c.kind IN ('album','album_view')),
 (SELECT COUNT(*) FROM audit_events WHERE event_type='album_view_recorded' AND resource_id='view-a'),
 (SELECT COUNT(*) FROM mutation_receipts WHERE operation='album.view.record')`).Scan(&values[0], &values[1], &values[2], &values[3], &values[4], &values[5], &values[6], &values[7]); err != nil {
		t.Fatal(err)
	}
	return values
}

func TestAlbumViewsSeparateActorsReceiptsAndFreshVisibility(t *testing.T) {
	f := newCollectionFixture(t)
	seedAlbumViews(t, f)
	ctx := t.Context()
	initial, err := f.service.AlbumView(ctx, f.other, "view-a")
	if err != nil || initial.ViewCount != 0 || initial.LastViewedAt != nil || initial.Version != 1 {
		t.Fatal("never-viewed state was not explicit", initial, err)
	}
	key := store.NewID()
	one, err := f.service.RecordAlbumView(ctx, f.other, "view-a", key)
	if err != nil || one.ViewCount != 1 || one.Version != 2 || one.LastViewedAt == nil {
		t.Fatal("first server-timed view was not recorded", one, err)
	}
	if got := albumViewState(t, f); got != [8]int64{1, 1, 2, 1, 1, 2, 0, 1} {
		t.Fatal("first view did not atomically publish personal and shared state", got)
	}
	replay, err := f.service.RecordAlbumView(ctx, f.other, "view-a", key)
	if err != nil || !reflect.DeepEqual(replay, one) || albumViewState(t, f) != [8]int64{1, 1, 2, 1, 1, 2, 0, 1} {
		t.Fatal("same-key replay counted a duplicate event", replay, one, err)
	}
	if _, err := f.service.RecordAlbumView(ctx, f.other, "view-b", key); !errors.Is(err, store.ErrIdempotencyReuse) {
		t.Fatal("same actor reused a key across albums", err)
	}
	ownerView, err := f.service.RecordAlbumView(ctx, f.owner, "view-a", key)
	if err != nil || ownerView.ViewCount != 1 || ownerView.Version != 2 {
		t.Fatal("another actor could not use the same private key", ownerView, err)
	}
	two, err := f.service.RecordAlbumView(ctx, f.other, "view-a", store.NewID())
	if err != nil || two.ViewCount != 2 || two.Version != 3 || two.LastViewedAt == nil || two.LastViewedAt.Before(*one.LastViewedAt) {
		t.Fatal("new key did not produce the next personal view", two, err)
	}
	public, err := f.service.Album(ctx, f.other, "view-a")
	if err != nil || public.ViewCount != 3 || public.Version != 4 {
		t.Fatal("shared total or album version diverged from the personal counters", public, err)
	}
	var personalPayload, sharedPayload string
	if err := f.db.QueryRow(`SELECT payload_json FROM change_log WHERE kind='album_view' AND resource_id='view-a' AND actor_scope=? ORDER BY seq DESC LIMIT 1`, f.other.User.ScopeID).Scan(&personalPayload); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	var personal access.AlbumView
	if err := json.Unmarshal([]byte(personalPayload), &fields); err != nil || len(fields) != 4 || json.Unmarshal([]byte(personalPayload), &personal) != nil || !reflect.DeepEqual(personal, two) {
		t.Fatal("private view event is incomplete or in the wrong scope", personalPayload, err)
	}
	if err := f.db.QueryRow(`SELECT payload_json FROM change_log WHERE kind='album' AND resource_id='view-a' AND actor_scope='system' ORDER BY seq DESC LIMIT 1`).Scan(&sharedPayload); err != nil {
		t.Fatal(err)
	}
	var shared struct {
		ViewCount int64 `json:"view_count"`
		Version   int64 `json:"version"`
	}
	if err := json.Unmarshal([]byte(sharedPayload), &shared); err != nil || shared.ViewCount != 3 || shared.Version != 4 {
		t.Fatal("shared album event lost its current view total or version", sharedPayload, err)
	}
	forged := f.other
	forged.User = f.owner.User
	if got, err := f.service.AlbumView(ctx, forged, "view-a"); err != nil || !reflect.DeepEqual(got, two) {
		t.Fatal("view read trusted a forged user DTO", got, err)
	}
	if replay, err := f.service.RecordAlbumView(ctx, f.other, "view-a", key); err != nil || !reflect.DeepEqual(replay, one) {
		t.Fatal("receipt did not retain its historical personal state", replay, err)
	}
	if current, err := f.service.AlbumView(ctx, f.other, "view-a"); err != nil || !reflect.DeepEqual(current, two) {
		t.Fatal("historical replay changed the current counter", current, err)
	}
	if _, err := f.db.Exec(`DELETE FROM user_groups WHERE user_id=?`, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RecordAlbumView(ctx, f.other, "view-a", key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("old receipt bypassed a revoked library grant", err)
	}
	if _, err := f.service.AlbumView(ctx, f.other, "view-a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("private view state bypassed revoked album visibility", err)
	}
	if _, err := f.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.AlbumView(ctx, forged, "view-a"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("disabled user retained authority through a forged DTO", err)
	}
}

func TestAlbumViewsRollBackEveryLateFailureAndDeduplicateConcurrency(t *testing.T) {
	f := newCollectionFixture(t)
	seedAlbumViews(t, f)
	ctx := t.Context()
	for _, trigger := range []string{
		`CREATE TRIGGER fail_view BEFORE INSERT ON change_log WHEN NEW.kind='album' AND NEW.resource_id='view-a' BEGIN SELECT RAISE(ABORT,'injected shared change failure'); END`,
		`CREATE TRIGGER fail_view BEFORE INSERT ON outbox_events WHEN EXISTS(SELECT 1 FROM change_log c WHERE c.seq=NEW.change_seq AND c.kind='album' AND c.resource_id='view-a') BEGIN SELECT RAISE(ABORT,'injected shared outbox failure'); END`,
		`CREATE TRIGGER fail_view BEFORE INSERT ON change_log WHEN NEW.kind='album_view' AND NEW.resource_id='view-a' BEGIN SELECT RAISE(ABORT,'injected private change failure'); END`,
		`CREATE TRIGGER fail_view BEFORE INSERT ON outbox_events WHEN EXISTS(SELECT 1 FROM change_log c WHERE c.seq=NEW.change_seq AND c.kind='album_view' AND c.resource_id='view-a') BEGIN SELECT RAISE(ABORT,'injected private outbox failure'); END`,
		`CREATE TRIGGER fail_view BEFORE INSERT ON mutation_receipts WHEN NEW.operation='album.view.record' BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`,
	} {
		if _, err := f.db.Exec(trigger); err != nil {
			t.Fatal(err)
		}
		before := albumViewState(t, f)
		if _, err := f.service.RecordAlbumView(ctx, f.other, "view-a", store.NewID()); err == nil {
			t.Fatal("late view failure was ignored", trigger)
		}
		if after := albumViewState(t, f); after != before {
			t.Fatal("late view failure leaked counters, events, audit or receipt", trigger, before, after)
		}
		if _, err := f.db.Exec(`DROP TRIGGER fail_view`); err != nil {
			t.Fatal(err)
		}
	}
	key := store.NewID()
	start := make(chan struct{})
	results := make(chan struct {
		value access.AlbumView
		err   error
	}, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			<-start
			value, err := f.service.RecordAlbumView(ctx, f.other, "view-a", key)
			results <- struct {
				value access.AlbumView
				err   error
			}{value, err}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	var first access.AlbumView
	for result := range results {
		if result.err != nil || first.Version != 0 && !reflect.DeepEqual(result.value, first) {
			t.Fatal("concurrent same-key view duplicated or changed its receipt", result, first)
		}
		first = result.value
	}
	if got := albumViewState(t, f); got != [8]int64{1, 1, 2, 1, 1, 2, 0, 1} {
		t.Fatal("concurrent replay produced more than one accepted view", got)
	}
	start = make(chan struct{})
	errs := make(chan error, 8)
	for range 8 {
		workers.Go(func() {
			<-start
			_, err := f.service.RecordAlbumView(ctx, f.other, "view-a", store.NewID())
			errs <- err
		})
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("distinct concurrent views did not serialize", err)
		}
	}
	if got := albumViewState(t, f); got != [8]int64{9, 9, 10, 9, 9, 18, 0, 9} {
		t.Fatal("distinct concurrent views lost an increment or transactional record", got)
	}
}

func TestAlbumViewsSharedTotalOverflowDoesNotChangeAnotherActor(t *testing.T) {
	f := newCollectionFixture(t)
	seedAlbumViews(t, f)
	if _, err := f.db.Exec(`INSERT INTO album_views(user_id,album_id,view_count,last_viewed_at,version)
 VALUES(?,'view-a',?,0,2),(?,'view-a',1,0,2)`, f.owner.User.ID, int64(math.MaxInt64-1), f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	before := albumViewState(t, f)
	if before != [8]int64{math.MaxInt64, math.MaxInt64, 1, 0, 0, 0, 0, 0} {
		t.Fatal("shared overflow fixture is invalid", before)
	}
	if _, err := f.service.RecordAlbumView(t.Context(), f.other, "view-a", store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("a full shared total accepted another actor's increment", err)
	}
	if after := albumViewState(t, f); after != before {
		t.Fatal("shared total overflow leaked a personal, shared or receipt change", before, after)
	}
	own, err := f.service.AlbumView(t.Context(), f.other, "view-a")
	if err != nil || own.ViewCount != 1 || own.Version != 2 {
		t.Fatal("shared total overflow changed the other account's personal state", own, err)
	}
}

func TestAlbumViewsConcurrentAccountDeletionAndViewAreSerialized(t *testing.T) {
	f := newCollectionFixture(t)
	seedAlbumViews(t, f)
	start := make(chan struct{})
	viewResult := make(chan error, 1)
	deleteResult := make(chan error, 1)
	go func() {
		<-start
		_, err := f.service.RecordAlbumView(t.Context(), f.other, "view-a", store.NewID())
		viewResult <- err
	}()
	go func() {
		<-start
		deleteResult <- f.auth.DeleteUser(t.Context(), f.owner, f.other.User.ID, f.other.User.Version)
	}()
	close(start)
	viewErr, deleteErr := <-viewResult, <-deleteResult
	if deleteErr != nil || viewErr != nil && !errors.Is(viewErr, auth.ErrUnauthenticated) {
		t.Fatal("concurrent deletion or view did not serialize under fresh authority", viewErr, deleteErr)
	}
	want := [8]int64{0, 0, 1, 0, 0, 0, 0, 0}
	if viewErr == nil {
		want = [8]int64{0, 0, 3, 2, 1, 3, 0, 0}
	}
	if got := albumViewState(t, f); got != want {
		t.Fatal("concurrent deletion retained a personal count, receipt or stale shared total", got, want)
	}
	if _, err := f.service.AlbumView(t.Context(), f.other, "view-a"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("deleted account retained a usable session", err)
	}
}
